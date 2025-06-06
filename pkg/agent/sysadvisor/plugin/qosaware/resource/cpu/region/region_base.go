/*
Copyright 2022 The Katalyst Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package region

import (
	"context"
	"fmt"
	"math"
	"sync"

	"go.uber.org/atomic"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"

	"github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	configapi "github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	workloadv1alpha1 "github.com/kubewharf/katalyst-api/pkg/apis/workload/v1alpha1"
	"github.com/kubewharf/katalyst-api/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/commonstate"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/metacache"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/qosaware/resource/cpu/region/headroompolicy"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/qosaware/resource/cpu/region/provisionpolicy"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/qosaware/resource/cpu/region/regulator"
	borweinctrl "github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/qosaware/resource/helper/modelctrl/borwein"
	"github.com/kubewharf/katalyst-core/pkg/util/cgroup/common"
	cgroupmgr "github.com/kubewharf/katalyst-core/pkg/util/cgroup/manager"

	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/types"
	"github.com/kubewharf/katalyst-core/pkg/config"
	pkgconsts "github.com/kubewharf/katalyst-core/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/metaserver"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	"github.com/kubewharf/katalyst-core/pkg/util/native"
)

const (
	metricCPUGetHeadroomFailed             = "get_cpu_headroom_failed"
	metricCPUGetProvisionFailed            = "get_cpu_provision_failed"
	metricRegionHeadroom                   = "region_headroom"
	metricCPUProvisionControlKnobRaw       = "cpu_provision_control_knob_raw"
	metricCPUProvisionControlKnobRegulated = "cpu_provision_control_knob_regulated"

	metricTagKeyPolicyName        = "policy_name"
	metricTagKeyRegionType        = "region_type"
	metricTagKeyRegionName        = "region_name"
	metricTagKeyRegionNUMAs       = "region_numas"
	metricTagKeyControlKnobName   = "control_knob_name"
	metricTagKeyControlKnobAction = "control_knob_action"
)

type internalPolicyState struct {
	updateStatus types.PolicyUpdateStatus
	initDoOnce   sync.Once
}

type internalProvisionPolicy struct {
	name   types.CPUProvisionPolicyName
	policy provisionpolicy.ProvisionPolicy
	internalPolicyState
}

type internalHeadroomPolicy struct {
	name   types.CPUHeadroomPolicyName
	policy headroompolicy.HeadroomPolicy
	internalPolicyState
}

type provisionPolicyResult struct {
	msg                        string
	essentials                 types.ResourceEssentials
	regulatorOptions           regulator.RegulatorOptions
	controlKnobValueRegulators map[v1alpha1.ControlKnobName]regulator.Regulator
}

func newProvisionPolicyResult(essentials types.ResourceEssentials, regulatorOptions regulator.RegulatorOptions, msg string) *provisionPolicyResult {
	return &provisionPolicyResult{
		msg:                        msg,
		essentials:                 essentials,
		regulatorOptions:           regulatorOptions,
		controlKnobValueRegulators: make(map[v1alpha1.ControlKnobName]regulator.Regulator),
	}
}

// setEssentials is to set essentials for each control knob
func (r *provisionPolicyResult) setEssentials(essentials types.ResourceEssentials) {
	r.essentials = essentials
	for _, reg := range r.controlKnobValueRegulators {
		reg.SetEssentials(essentials)
	}
}

// regulateControlKnob is to regulate control knob with current and last one
// todo: current only regulate control knob value, it will also regulate action in the future
func (r *provisionPolicyResult) regulateControlKnob(currentControlKnob, effectiveControlKnob types.ControlKnob) {
	klog.InfoS("[provisionPolicyResult]", "region", r.msg,
		"currentControlKnob", currentControlKnob, "effectiveControlKnob", effectiveControlKnob)
	for name, knob := range currentControlKnob {
		reg, ok := r.controlKnobValueRegulators[name]
		if !ok || reg == nil {
			reg = r.newRegulator(name)
		}
		effectiveKnobItem, ok := effectiveControlKnob[name]
		if ok {
			reg.Regulate(knob, &effectiveKnobItem)
		} else {
			reg.Regulate(knob, nil)
		}
		r.controlKnobValueRegulators[name] = reg
	}
	// cleanup control knob regulators
	for name := range r.controlKnobValueRegulators {
		if _, ok := currentControlKnob[name]; !ok {
			delete(r.controlKnobValueRegulators, name)
		}
	}
}

// newRegulator new regulator according to the control knob name
func (r *provisionPolicyResult) newRegulator(name v1alpha1.ControlKnobName) regulator.Regulator {
	switch name {
	// only non-reclaimed cpu size need regulate now
	case v1alpha1.ControlKnobNonReclaimedCPURequirement:
		return regulator.NewCPURegulator(r.essentials, r.regulatorOptions)
	default:
		return regulator.NewDummyRegulator()
	}
}

// getControlKnob is to get final control knob from regulators
func (r *provisionPolicyResult) getControlKnob() types.ControlKnob {
	controlKnob := make(types.ControlKnob)
	for name, regulator := range r.controlKnobValueRegulators {
		controlKnob[name] = types.ControlKnobItem{
			Value:  float64(regulator.GetRequirement()),
			Action: types.ControlKnobActionNone,
		}
	}
	return controlKnob
}

type QoSRegionBase struct {
	sync.Mutex
	conf *config.Configuration

	name          string
	ownerPoolName string
	regionType    v1alpha1.QoSRegionType
	regionStatus  types.RegionStatus

	types.ResourceEssentials
	types.ControlEssentials

	// bindingNumas records numas assigned to this region
	bindingNumas machine.CPUSet
	// podSet records current pod and containers in region keyed by pod uid and container name
	podSet types.PodSet
	// containerTopologyAwareAssignment changes dynamically by adding container
	containerTopologyAwareAssignment types.TopologyAwareAssignment
	// indicatorCurrentGetters stores metrics getters for indicators interested in
	indicatorCurrentGetters map[string]types.IndicatorCurrentGetter

	// provisionPolicies for comparing and merging different provision policy results,
	// the former has higher priority; provisionPolicyNameInUse indicates the provision
	// policy in-use currently
	provisionPolicies        []*internalProvisionPolicy
	provisionPolicyNameInUse types.CPUProvisionPolicyName
	provisionPolicyResults   map[types.CPUProvisionPolicyName]*provisionPolicyResult

	// ctrl knob need policy restrict
	ctrlKnobsNeedPolicyRestrict map[v1alpha1.ControlKnobName]bool

	// cpuRegulatorOptions is the regulator options for cpu regulator
	cpuRegulatorOptions regulator.RegulatorOptions

	// headroomPolicies for comparing and merging different headroom policy results,
	// the former has higher priority; headroomPolicyNameInUse indicates the headroom
	// policy in-use currently
	headroomPolicies        []*internalHeadroomPolicy
	headroomPolicyNameInUse types.CPUHeadroomPolicyName

	metaReader metacache.MetaReader
	metaServer *metaserver.MetaServer
	emitter    metrics.MetricEmitter

	// borweinController will take effect only when using rama provision policy.
	borweinController *borweinctrl.BorweinController

	// enableReclaim returns true if the resources of region can be reclaimed to supply for reclaimed_cores
	enableReclaim func() bool

	// throttled: true if unable to reach the ResourceUpperBound due to competition for resources with other regions.
	throttled atomic.Bool

	// idle: true if containers in the region is not running as usual, maybe there is no incoming business traffic
	idle atomic.Bool

	isNumaBinding bool
}

// NewQoSRegionBase returns a base qos region instance with common region methods
func NewQoSRegionBase(name string, ownerPoolName string, regionType v1alpha1.QoSRegionType, conf *config.Configuration, extraConf interface{}, isNumaBinding bool,
	metaReader metacache.MetaReader, metaServer *metaserver.MetaServer, emitter metrics.MetricEmitter,
) *QoSRegionBase {
	r := &QoSRegionBase{
		conf:          conf,
		name:          name,
		ownerPoolName: ownerPoolName,
		regionType:    regionType,

		bindingNumas:                     machine.NewCPUSet(),
		podSet:                           make(types.PodSet),
		containerTopologyAwareAssignment: make(types.TopologyAwareAssignment),

		provisionPolicies: make([]*internalProvisionPolicy, 0),
		headroomPolicies:  make([]*internalHeadroomPolicy, 0),

		provisionPolicyNameInUse: types.CPUProvisionPolicyNone,
		headroomPolicyNameInUse:  types.CPUHeadroomPolicyNone,

		provisionPolicyResults: make(map[types.CPUProvisionPolicyName]*provisionPolicyResult),
		cpuRegulatorOptions: regulator.RegulatorOptions{
			MaxRampUpStep:     conf.MaxRampUpStep,
			MaxRampDownStep:   conf.MaxRampDownStep,
			MinRampDownPeriod: conf.MinRampDownPeriod,
			NeedHTAligned: func() bool {
				return machine.SmtActive() &&
					!conf.GetDynamicConfiguration().AllowSharedCoresOverlapReclaimedCores &&
					regionType == v1alpha1.QoSRegionTypeShare
			},
		},

		ctrlKnobsNeedPolicyRestrict: map[v1alpha1.ControlKnobName]bool{configapi.ControlKnobReclaimedCoresCPUQuota: true},

		metaReader: metaReader,
		metaServer: metaServer,
		emitter:    emitter,

		throttled: *atomic.NewBool(false),
		idle:      *atomic.NewBool(false),

		isNumaBinding: isNumaBinding,
	}

	r.initHeadroomPolicy(conf, extraConf, metaReader, metaServer, emitter)
	r.initProvisionPolicy(conf, extraConf, metaReader, metaServer, emitter)

	// enableBorweinModel is initialized according to rama provision policy config.
	// it only takes effect when updating target indicators,
	// if there are more code positions depending on it,
	// we should consider provide a dummy borwein controller to avoid redundant judgement.
	if r.conf.PolicyRama.EnableBorwein {
		r.borweinController = borweinctrl.NewBorweinController(name, regionType, ownerPoolName, conf, metaReader, emitter)
	}
	r.enableReclaim = r.EnableReclaimFunc

	klog.Infof("[qosaware-cpu] created region [%v/%v/%v]", r.Name(), r.Type(), r.OwnerPoolName())

	return r
}

func (r *QoSRegionBase) Name() string {
	return r.name
}

// TryUpdateProvision is implemented in specific region
func (r *QoSRegionBase) TryUpdateProvision() {}

func (r *QoSRegionBase) Type() v1alpha1.QoSRegionType {
	return r.regionType
}

func (r *QoSRegionBase) OwnerPoolName() string {
	return r.ownerPoolName
}

func (r *QoSRegionBase) IsEmpty() bool {
	r.Lock()
	defer r.Unlock()

	return len(r.podSet) <= 0
}

func (r *QoSRegionBase) Clear() {
	r.Lock()
	defer r.Unlock()

	r.podSet = make(types.PodSet)
	r.containerTopologyAwareAssignment = make(types.TopologyAwareAssignment)
}

func (r *QoSRegionBase) GetMetaInfo() string {
	r.Lock()
	defer r.Unlock()
	return r.getMetaInfo()
}

func (r *QoSRegionBase) getMetaInfo() string {
	return fmt.Sprintf("[regionName: %s, regionType: %s, ownerPoolName: %s, NUMAs: %v]", r.name, r.regionType, r.ownerPoolName, r.bindingNumas.String())
}

func (r *QoSRegionBase) GetBindingNumas() machine.CPUSet {
	r.Lock()
	defer r.Unlock()

	return r.bindingNumas.Clone()
}

func (r *QoSRegionBase) GetPods() types.PodSet {
	r.Lock()
	defer r.Unlock()

	return r.podSet.Clone()
}

func (r *QoSRegionBase) GetPodsRequest() float64 {
	r.Lock()
	defer r.Unlock()

	return r.getPodsRequest()
}

func (r *QoSRegionBase) getPodsRequest() float64 {
	requests := .0
	for podUID := range r.podSet {
		if pod, err := r.metaServer.GetPod(context.TODO(), podUID); err == nil {
			reqs := native.SumUpPodRequestResources(pod)
			cpuReq, ok := reqs[v1.ResourceCPU]
			if ok {
				requests += float64(cpuReq.MilliValue()) / 1000
			}
		}
	}

	return requests
}

func (r *QoSRegionBase) SetBindingNumas(numas machine.CPUSet) {
	r.Lock()
	defer r.Unlock()

	r.bindingNumas = numas
}

func (r *QoSRegionBase) SetEssentials(essentials types.ResourceEssentials) {
	r.Lock()
	defer r.Unlock()

	r.ResourceEssentials = essentials
}

func (r *QoSRegionBase) SetThrottled(throttled bool) {
	r.throttled.Store(throttled)
}

func (r *QoSRegionBase) IsNumaBinding() bool {
	return r.isNumaBinding
}

func (r *QoSRegionBase) AddContainer(ci *types.ContainerInfo) error {
	r.Lock()
	defer r.Unlock()

	if ci == nil {
		return fmt.Errorf("container info nil")
	}

	r.podSet.Insert(ci.PodUID, ci.ContainerName)

	if len(r.containerTopologyAwareAssignment) <= 0 {
		r.containerTopologyAwareAssignment = ci.TopologyAwareAssignments.Clone()
	} else {
		// Sanity check: all containers in the region share the same cpuset
		// Do not return error when sanity check fails to prevent unnecessary stall
		if !r.containerTopologyAwareAssignment.Equals(ci.TopologyAwareAssignments) {
			klog.Warningf("[qosaware-cpu] sanity check failed")
		}
	}

	return nil
}

func (r *QoSRegionBase) TryUpdateHeadroom() {
	r.Lock()
	defer r.Unlock()

	for _, internal := range r.headroomPolicies {
		internal.updateStatus = types.PolicyUpdateFailed

		// set essentials for policy
		internal.policy.SetPodSet(r.podSet)
		internal.policy.SetBindingNumas(r.bindingNumas)
		internal.policy.SetEssentials(r.ResourceEssentials)

		// run an episode of policy and calculator update
		if err := internal.policy.Update(); err != nil {
			klog.Errorf("[qosaware-cpu] region %s update policy %v failed: %v", r.name, internal.name, err)
			continue
		}
		internal.updateStatus = types.PolicyUpdateSucceeded
	}
}

func (r *QoSRegionBase) GetProvision() (types.ControlKnob, error) {
	r.Lock()
	defer r.Unlock()

	oldProvisionPolicyNameInUse := r.provisionPolicyNameInUse
	r.provisionPolicyNameInUse = types.CPUProvisionPolicyNone

	for _, internal := range r.provisionPolicies {
		if internal.updateStatus != types.PolicyUpdateSucceeded {
			_ = r.emitter.StoreInt64(metricCPUGetProvisionFailed, 1, metrics.MetricTypeNameRaw,
				metrics.MetricTag{Key: metricTagKeyPolicyName, Val: string(internal.name)})
			continue
		}

		result, ok := r.provisionPolicyResults[internal.name]
		if !ok {
			klog.Errorf("[qosaware-cpu] get control knob by policy %v failed", internal.name)
			continue
		}
		r.provisionPolicyNameInUse = internal.name

		if r.provisionPolicyNameInUse != oldProvisionPolicyNameInUse {
			klog.Infof("[qosaware-cpu] region: %v provision policy switch from %v to %v",
				r.Name(), oldProvisionPolicyNameInUse, r.provisionPolicyNameInUse)
			if r.conf.PolicyRama.EnableBorwein {
				r.borweinController.ResetIndicatorOffsets()
			}
		}
		// TODO: return ctrl knobs of all policies
		return result.getControlKnob(), nil
	}

	return nil, fmt.Errorf("failed to get legal provision")
}

func (r *QoSRegionBase) GetHeadroom() (float64, error) {
	r.Lock()
	defer r.Unlock()

	r.headroomPolicyNameInUse = types.CPUHeadroomPolicyNone

	for _, internal := range r.headroomPolicies {
		if internal.updateStatus != types.PolicyUpdateSucceeded {
			_ = r.emitter.StoreInt64(metricCPUGetHeadroomFailed, 1, metrics.MetricTypeNameRaw,
				metrics.MetricTag{Key: metricTagKeyPolicyName, Val: string(internal.name)})
			continue
		}
		headroom, err := internal.policy.GetHeadroom()
		if err != nil {
			klog.Errorf("[qosaware-cpu] get headroom by policy %v failed: %v", internal.name, err)
			continue
		}
		_ = r.emitter.StoreFloat64(metricRegionHeadroom, headroom, metrics.MetricTypeNameRaw,
			metrics.ConvertMapToTags(map[string]string{
				metricTagKeyRegionType: string(r.regionType),
				metricTagKeyRegionName: r.name, metricTagKeyPolicyName: string(internal.name),
				metricTagKeyRegionNUMAs: r.bindingNumas.String(),
			})...)
		r.headroomPolicyNameInUse = internal.name
		return headroom, nil
	}

	return 0, fmt.Errorf("failed to get valid headroom")
}

func (r *QoSRegionBase) GetProvisionPolicy() (policyTopPriority types.CPUProvisionPolicyName, policyInUse types.CPUProvisionPolicyName) {
	r.Lock()
	defer r.Unlock()

	policyTopPriority = types.CPUProvisionPolicyNone
	if len(r.provisionPolicies) > 0 {
		policyTopPriority = r.provisionPolicies[0].name
	}

	if !r.enableReclaim() {
		policyInUse = types.CPUProvisionPolicyNonReclaim
	} else {
		policyInUse = r.provisionPolicyNameInUse
	}

	return
}

func (r *QoSRegionBase) EnableReclaim() bool {
	r.Lock()
	defer r.Unlock()

	return r.enableReclaim()
}

func (r *QoSRegionBase) GetHeadRoomPolicy() (policyTopPriority types.CPUHeadroomPolicyName, policyInUse types.CPUHeadroomPolicyName) {
	r.Lock()
	defer r.Unlock()

	policyTopPriority = types.CPUHeadroomPolicyNone
	if len(r.headroomPolicies) > 0 {
		policyTopPriority = r.headroomPolicies[0].name
	}

	if !r.enableReclaim() {
		policyInUse = types.CPUHeadroomPolicyNonReclaim
	} else {
		policyInUse = r.headroomPolicyNameInUse
	}

	return
}

func (r *QoSRegionBase) GetStatus() types.RegionStatus {
	r.Lock()
	defer r.Unlock()

	return r.regionStatus.Clone()
}

func (r *QoSRegionBase) GetControlEssentials() types.ControlEssentials {
	r.Lock()
	defer r.Unlock()

	return r.ControlEssentials
}

// getRegionNameFromMetaCache returns region name owned by container from metacache,
// to restore region info after restart. If numaID is specified, binding numas of the
// region will be checked, otherwise only one region should be owned by container.
func getRegionNameFromMetaCache(ci *types.ContainerInfo, numaID int, metaReader metacache.MetaReader) string {
	if ci.QoSLevel == consts.PodAnnotationQoSLevelSharedCores {
		if len(ci.RegionNames) == 1 {
			// get region name from metaCache
			regionName := ci.RegionNames.List()[0]
			regionInfo, ok := metaReader.GetRegionInfo(regionName)
			if ok {
				// the region-name is valid if it suits it follows constrains below
				// - current container is numa-binding and the region is for numa-binding and
				//   the region's binding numa is the same as the container's numaID
				// - current container is isolated and the region is for isolation-type
				// - current container isn't isolated and the region is for share-type

				if ci.IsNumaBinding() {
					regionNUMAs := regionInfo.BindingNumas.ToSliceInt()
					if len(regionNUMAs) != 1 || regionNUMAs[0] != numaID {
						return ""
					}
				}

				if !ci.Isolated && regionInfo.RegionType == v1alpha1.QoSRegionTypeShare {
					return regionName
				} else if ci.Isolated && regionInfo.RegionType == v1alpha1.QoSRegionTypeIsolation {
					return regionName
				}
			}
		}
	} else if ci.IsDedicatedNumaBinding() {
		for regionName := range ci.RegionNames {
			regionInfo, ok := metaReader.GetRegionInfo(regionName)
			if ok && regionInfo.RegionType == v1alpha1.QoSRegionTypeDedicatedNumaExclusive {
				regionNUMAs := regionInfo.BindingNumas.ToSliceInt()
				if len(regionNUMAs) == 1 && regionNUMAs[0] == numaID {
					return regionName
				}
			}
		}
	}
	return ""
}

// initProvisionPolicy initializes provision by adding additional policies into default ones
func (r *QoSRegionBase) initProvisionPolicy(conf *config.Configuration, extraConf interface{},
	metaReader metacache.MetaReader, metaServer *metaserver.MetaServer, emitter metrics.MetricEmitter,
) {
	configuredProvisionPolicy, ok := conf.CPUAdvisorConfiguration.ProvisionPolicies[r.regionType]
	if !ok {
		klog.Warningf("[qosaware-cpu] failed to find provision policies for region %v", r.regionType)
		return
	}

	// try new policies
	// todo move to separate functions
	initializers := provisionpolicy.GetRegisteredInitializers()
	for _, policyName := range configuredProvisionPolicy {
		if initializer, ok := initializers[policyName]; ok {
			policy := initializer(r.name, r.regionType, r.ownerPoolName, conf, extraConf, metaReader, metaServer, emitter)
			policy.SetBindingNumas(r.bindingNumas, false)
			r.provisionPolicies = append(r.provisionPolicies, &internalProvisionPolicy{
				name:                policyName,
				policy:              policy,
				internalPolicyState: internalPolicyState{updateStatus: types.PolicyUpdateFailed},
			})
		} else {
			general.ErrorS(fmt.Errorf("failed to find region policy"), "policyName", policyName, "region", r.regionType)
		}
	}
}

// initHeadroomPolicy initializes headroom by adding additional policies into default ones
func (r *QoSRegionBase) initHeadroomPolicy(conf *config.Configuration, extraConf interface{},
	metaReader metacache.MetaReader, metaServer *metaserver.MetaServer, emitter metrics.MetricEmitter,
) {
	configuredHeadroomPolicy, ok := conf.CPUAdvisorConfiguration.HeadroomPolicies[r.regionType]
	if !ok {
		klog.Warningf("[qosaware-cpu] failed to find headroom policies for region %v", r.regionType)
		return
	}

	// try new policies
	headroomInitializers := headroompolicy.GetRegisteredInitializers()
	for _, policyName := range configuredHeadroomPolicy {
		if initializer, ok := headroomInitializers[policyName]; ok {
			policy := initializer(r.name, r.regionType, r.ownerPoolName, conf, extraConf, metaReader, metaServer, emitter)
			r.headroomPolicies = append(r.headroomPolicies, &internalHeadroomPolicy{
				name:                policyName,
				policy:              policy,
				internalPolicyState: internalPolicyState{updateStatus: types.PolicyUpdateFailed},
			})
		} else {
			general.ErrorS(fmt.Errorf("failed to find headroom policy"), "policyName", policyName, "region", r.regionType)
		}
	}
}

// getProvisionControlKnob get current adjusted control knobs for each policy
func (r *QoSRegionBase) getProvisionControlKnob() map[types.CPUProvisionPolicyName]types.ControlKnob {
	provisionControlKnob := make(map[types.CPUProvisionPolicyName]types.ControlKnob)
	for _, internal := range r.provisionPolicies {
		if internal.updateStatus != types.PolicyUpdateSucceeded {
			continue
		}

		controlKnob, err := internal.policy.GetControlKnobAdjusted()
		if err != nil {
			klog.Errorf("[qosaware-cpu] get control knob by policy %v failed: %v", internal.name, err)
			continue
		}

		provisionControlKnob[internal.name] = controlKnob

		for name, value := range controlKnob {
			_ = r.emitter.StoreFloat64(metricCPUProvisionControlKnobRaw, value.Value, metrics.MetricTypeNameRaw, []metrics.MetricTag{
				{Key: metricTagKeyRegionType, Val: string(r.regionType)},
				{Key: metricTagKeyRegionName, Val: r.name},
				{Key: metricTagKeyPolicyName, Val: string(internal.name)},
				{Key: metricTagKeyControlKnobName, Val: string(name)},
				{Key: metricTagKeyControlKnobAction, Val: string(value.Action)},
			}...)

			klog.InfoS("[qosaware-cpu] get raw control knob", "meta", r.getMetaInfo(), "policy", internal.name,
				"knob", name, "action", value.Action, "value", value.Value)
		}
	}

	return provisionControlKnob
}

// regulateProvisionControlKnob regulate provision control knob for each provision policy
func (r *QoSRegionBase) regulateProvisionControlKnob(originControlKnob map[types.CPUProvisionPolicyName]types.ControlKnob,
	effectiveControlKnob types.ControlKnob,
) {
	provisionPolicyResults := make(map[types.CPUProvisionPolicyName]*provisionPolicyResult)
	firstValidPolicy := types.CPUProvisionPolicyNone
	for _, internal := range r.provisionPolicies {
		if internal.updateStatus != types.PolicyUpdateSucceeded {
			continue
		}

		if originControlKnob == nil {
			continue
		}

		controlKnob, ok := originControlKnob[internal.name]
		if !ok {
			continue
		}

		if firstValidPolicy == types.CPUProvisionPolicyNone {
			firstValidPolicy = internal.name
		}

		policyResult, ok := r.provisionPolicyResults[internal.name]
		if !ok || policyResult == nil {
			policyResult = newProvisionPolicyResult(r.ResourceEssentials, r.cpuRegulatorOptions, r.getMetaInfo())
			policyResult.regulateControlKnob(controlKnob, effectiveControlKnob)
		} else {
			policyResult.setEssentials(r.ResourceEssentials)
			// only set regulator last cpu requirement for first valid policy
			if internal.name == firstValidPolicy {
				policyResult.regulateControlKnob(controlKnob, effectiveControlKnob)
			} else {
				policyResult.regulateControlKnob(controlKnob, nil)
			}
		}

		provisionPolicyResults[internal.name] = policyResult
	}

	r.provisionPolicyResults = provisionPolicyResults
	for policy, result := range r.provisionPolicyResults {
		for knob, value := range result.getControlKnob() {
			_ = r.emitter.StoreFloat64(metricCPUProvisionControlKnobRegulated, value.Value, metrics.MetricTypeNameRaw, []metrics.MetricTag{
				{Key: metricTagKeyRegionType, Val: string(r.regionType)},
				{Key: metricTagKeyRegionName, Val: r.name},
				{Key: metricTagKeyPolicyName, Val: string(policy)},
				{Key: metricTagKeyControlKnobName, Val: string(knob)},
				{Key: metricTagKeyControlKnobAction, Val: string(value.Action)},
			}...)
			klog.InfoS("[qosaware-cpu] get regulated control knob", "region", r.name, "bindingNumas", r.bindingNumas.String(),
				"policy", policy, "knob", knob, "action", value.Action, "value", value.Value)
		}
	}
}

// getIndicators returns indicator targets from spd and current by region specific indicator getters
func (r *QoSRegionBase) getIndicators() (types.Indicator, error) {
	ctx := context.Background()
	indicatorTargetConfig, ok := r.conf.GetDynamicConfiguration().RegionIndicatorTargetConfiguration[r.regionType]
	if !ok {
		return nil, fmt.Errorf("get %v indicators failed", r.regionType)
	}

	general.Infof("indicatorTargetConfig: %v, region %v", indicatorTargetConfig, r.name)

	indicators := make(types.Indicator)
	for _, indicator := range indicatorTargetConfig {
		indicatorName := indicator.Name
		defaultTarget := indicator.Target
		indicatorCurrentGetter, ok := r.indicatorCurrentGetters[string(indicatorName)]
		if !ok {
			general.InfoS("failed to find indicatorCurrentGetter", "indicatorName", indicatorName)
			continue
		}

		current, err := indicatorCurrentGetter()
		if err != nil {
			return nil, err
		}

		target := defaultTarget
		if len(r.podSet) > 0 {
			minTarget := math.MaxFloat64
			sumTarget := 0.0
			for podUID := range r.podSet {
				indicatorTarget, err := r.getPodIndicatorTarget(ctx, podUID, string(indicatorName), defaultTarget)
				if err != nil || indicatorTarget == nil {
					indicatorTarget = &defaultTarget
					general.Warningf("use default indicator target[%v],because of failed to get indicator %s of poduid[%s] err: %v",
						defaultTarget, indicatorName, podUID, err)
				}
				sumTarget = sumTarget + *indicatorTarget
				minTarget = math.Min(minTarget, *indicatorTarget)
			}
			avgTarget := sumTarget / float64(len(r.podSet))
			switch indicatorName {
			case workloadv1alpha1.ServiceSystemIndicatorNameCPUUsageRatio:
				// get avg target for cpu_usage_ratio
				target = avgTarget
			default:
				// get min target by default
				target = minTarget
			}
		}
		indicatorValue := types.IndicatorValue{
			Current: current,
			Target:  target,
		}

		if indicatorValue.Target <= 0 || indicatorValue.Current <= 0 {
			klog.ErrorS(nil, "invalid indicator", "indicatorName", indicatorName, "indicatorValue", indicatorValue)
			continue
		}

		indicators[string(indicatorName)] = indicatorValue
	}
	if r.conf.PolicyRama.EnableBorwein && r.provisionPolicyNameInUse == types.CPUProvisionPolicyRama {
		general.Infof("try to update indicators by borwein model")
		return r.borweinController.GetUpdatedIndicators(indicators, r.podSet), nil
	} else {
		return indicators, nil
	}
}

// getPodIndicatorTarget gets pod indicator target by given pod uid and indicator name,
// if no performance target or indicator is found, it will return default target
func (r *QoSRegionBase) getPodIndicatorTarget(ctx context.Context, podUID string,
	indicatorName string, defaultTarget float64,
) (*float64, error) {
	pod, err := r.metaServer.GetPod(ctx, podUID)
	if err != nil {
		return nil, err
	}

	indicatorTarget := defaultTarget
	servicePerformanceTarget, err := r.metaServer.ServiceSystemPerformanceTarget(ctx, pod.ObjectMeta)
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	} else if err != nil {
		return &indicatorTarget, nil
	}

	target, ok := servicePerformanceTarget[indicatorName]
	if !ok {
		return &indicatorTarget, nil
	}

	// get the indicator target in the following order:
	// 1. service indicator upperBound
	// 2. service indicator lowerBound
	// 3. default indicator target

	if target.LowerBound != nil {
		indicatorTarget = *target.LowerBound
	}

	if target.UpperBound != nil {
		indicatorTarget = *target.UpperBound
	}

	return &indicatorTarget, nil
}

// UpdateStatus updates region status based on resource and control essentials
func (r *QoSRegionBase) UpdateStatus() {
	overshoot := r.updateOvershootStatus()
	r.updateBoundType(overshoot)
}

func (r *QoSRegionBase) updateBoundType(overshoot bool) {
	// todo BoundType logic is kind of mess, need to refine this in the future
	boundType := types.BoundUnknown
	resourceUpperBoundHit := false
	if r.IsThrottled() {
		general.Infof("region %v is throttled", r.name)
		boundType = types.BoundUpper
	} else if r.ControlEssentials.ControlKnobs != nil {
		if v, ok := r.ControlEssentials.ControlKnobs[v1alpha1.ControlKnobNonReclaimedCPURequirement]; ok {
			if v.Value <= r.ResourceEssentials.ResourceLowerBound {
				boundType = types.BoundLower
			} else {
				boundType = types.BoundNone
				if v.Value >= r.ResourceEssentials.ResourceUpperBound || !r.enableReclaim() {
					resourceUpperBoundHit = true
				}
			}
		}
	}

	if overshoot && resourceUpperBoundHit {
		general.Infof("region %v is overshoot and resourceUpperBoundHit", r.name)
		boundType = types.BoundUpper
	}
	// fill in bound entry
	r.regionStatus.BoundType = boundType
}

func (r *QoSRegionBase) updateOvershootStatus() bool {
	// reset entries
	r.regionStatus.OvershootStatus = make(map[string]types.OvershootType)

	for indicatorName := range r.indicatorCurrentGetters {
		r.regionStatus.OvershootStatus[indicatorName] = types.OvershootUnknown
	}
	overshoot := false

	// fill in overshoot entry
	for indicatorName, indicator := range r.ControlEssentials.Indicators {
		if indicator.Current > indicator.Target && !r.IsIdle() {
			overshoot = true
			r.regionStatus.OvershootStatus[indicatorName] = types.OvershootTrue
		} else {
			r.regionStatus.OvershootStatus[indicatorName] = types.OvershootFalse
		}
	}
	return overshoot
}

func (r *QoSRegionBase) EnableReclaimFunc() bool {
	return r.ResourceEssentials.EnableReclaim
}

func (r *QoSRegionBase) IsThrottled() bool {
	return r.throttled.Load()
}

func (r *QoSRegionBase) IsIdle() bool {
	return r.idle.Load()
}

// available for Intel
func (r *QoSRegionBase) getMemoryAccessWriteLatency() (float64, error) {
	latency := 0.0
	for _, numaID := range r.bindingNumas.ToSliceInt() {
		data, err := r.metaReader.GetNumaMetric(numaID, pkgconsts.MetricMemLatencyWriteNuma)
		if err != nil {
			return 0, err
		}
		latency = general.MaxFloat64(latency, data.Value)
	}

	return latency, nil
}

// available for Intel
func (r *QoSRegionBase) getMemoryAccessReadLatency() (float64, error) {
	latency := 0.0
	for _, numaID := range r.bindingNumas.ToSliceInt() {
		data, err := r.metaReader.GetNumaMetric(numaID, pkgconsts.MetricMemLatencyReadNuma)
		if err != nil {
			return 0, err
		}
		latency = math.Max(latency, data.Value)
	}

	return latency, nil
}

// available for AMD
func (r *QoSRegionBase) getMemoryL3MissLatency() (float64, error) {
	latency := 0.0
	for _, numaID := range r.bindingNumas.ToSliceInt() {
		data, err := r.metaReader.GetNumaMetric(numaID, pkgconsts.MetricMemAMDL3MissLatencyNuma)
		if err != nil {
			return 0, err
		}
		latency = math.Max(latency, data.Value)
	}

	return latency, nil
}

func (r *QoSRegionBase) getEffectiveReclaimResource() (quota float64, cpusetSize int, err error) {
	numaID := commonstate.FakedNUMAID
	if r.isNumaBinding {
		numaID = r.bindingNumas.ToSliceInt()[0]
	}

	quotaCtrlKnobEnabled, err := metacache.IsQuotaCtrlKnobEnabled(r.metaReader)
	if err != nil {
		return 0, 0, err
	}

	reclaimPath := common.GetReclaimRelativeRootCgroupPath(r.conf.ReclaimRelativeRootCgroupPath, numaID)
	cpuStats, err := cgroupmgr.GetCPUWithRelativePath(reclaimPath)
	if err != nil {
		return 0, 0, err
	}
	if cpuStats.CpuQuota == math.MaxInt || cpuStats.CpuQuota == common.CPUQuotaUnlimit || !quotaCtrlKnobEnabled {
		quota = common.CPUQuotaUnlimit
	} else {
		quota = float64(cpuStats.CpuQuota) / float64(cpuStats.CpuPeriod)
	}

	for _, numaID := range r.bindingNumas.ToSliceInt() {
		if reclaimedInfo, ok := r.metaReader.GetPoolInfo(commonstate.PoolNameReclaim); ok {
			cpusetSize += reclaimedInfo.TopologyAwareAssignments[numaID].Size()
		}
	}
	return
}

// restrictProvisionControlKnob is used to restrict provision control knob by reference policy
func (r *QoSRegionBase) restrictProvisionControlKnob(originControlKnob map[types.CPUProvisionPolicyName]types.ControlKnob) map[types.CPUProvisionPolicyName]types.ControlKnob {
	restrictedControlKnob := make(map[types.CPUProvisionPolicyName]types.ControlKnob)
	for policyName, controlKnob := range originControlKnob {
		restrictedControlKnob[policyName] = controlKnob.Clone()
		refPolicyName, ok := r.conf.RestrictRefPolicy[policyName]
		if !ok {
			continue
		}
		refControlKnob, ok := originControlKnob[refPolicyName]
		if !ok {
			klog.Errorf("get control knob from reference policy %v for policy %v failed", refPolicyName, policyName)
			continue
		}

		for controlKnobName, rawKnobValue := range controlKnob {
			needed, ok := r.ctrlKnobsNeedPolicyRestrict[controlKnobName]
			if !ok || !needed {
				continue
			}

			refKnobValue, ok := refControlKnob[controlKnobName]
			if !ok {
				continue
			}
			restrictedKnobValue := rawKnobValue
			if rawKnobValue.Value > refKnobValue.Value {
				restrictedKnobValue = refKnobValue

				klog.Infof("[qosaware-cpu] restrict control knob %v for policy %v by policy %v from %.2f to %.2f, refKnobValue: %v",
					controlKnobName, policyName, refPolicyName, rawKnobValue.Value, restrictedKnobValue.Value, refKnobValue.Value)
			}

			restrictedControlKnob[policyName][controlKnobName] = restrictedKnobValue
		}
	}
	return restrictedControlKnob
}
