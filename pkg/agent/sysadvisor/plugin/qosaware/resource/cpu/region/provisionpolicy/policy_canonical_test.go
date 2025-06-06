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

package provisionpolicy

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8types "k8s.io/apimachinery/pkg/types"

	"github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	apiconsts "github.com/kubewharf/katalyst-api/pkg/consts"
	katalyst_base "github.com/kubewharf/katalyst-core/cmd/base"
	"github.com/kubewharf/katalyst-core/cmd/katalyst-agent/app/options"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/metacache"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/types"
	"github.com/kubewharf/katalyst-core/pkg/config"
	provisionconfig "github.com/kubewharf/katalyst-core/pkg/config/agent/sysadvisor/qosaware/resource/cpu/provision"
	"github.com/kubewharf/katalyst-core/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/metaserver"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent/metric"
	metrictypes "github.com/kubewharf/katalyst-core/pkg/metaserver/agent/metric/types"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent/pod"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	metricspool "github.com/kubewharf/katalyst-core/pkg/metrics/metrics-pool"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	metricutil "github.com/kubewharf/katalyst-core/pkg/util/metric"
)

func generateCanonicalTestConfiguration(t *testing.T, checkpointDir, stateFileDir, checkpointManagerDir string) *config.Configuration {
	conf, err := options.NewOptions().Config()
	require.NoError(t, err)
	require.NotNil(t, conf)

	conf.GenericSysAdvisorConfiguration.StateFileDirectory = stateFileDir
	conf.MetaServerConfiguration.CheckpointManagerDir = checkpointDir
	conf.CheckpointManagerDir = checkpointManagerDir

	conf.GetDynamicConfiguration().RegionIndicatorTargetConfiguration = map[v1alpha1.QoSRegionType][]v1alpha1.IndicatorTargetConfiguration{
		v1alpha1.QoSRegionTypeShare: {
			{
				Name: consts.MetricCPUSchedwait,
			},
		},
		v1alpha1.QoSRegionTypeDedicatedNumaExclusive: {
			{
				Name: consts.MetricCPUCPIContainer,
			},
			{
				Name: consts.MetricMemBandwidthNuma,
			},
		},
	}

	conf.PolicyRama = &provisionconfig.PolicyRamaConfiguration{
		PIDParameters: map[string]types.FirstOrderPIDParams{
			consts.MetricCPUSchedwait: {
				Kpp:                  0.1,
				Kpn:                  0.01,
				Kdp:                  0.0,
				Kdn:                  0.0,
				AdjustmentUpperBound: types.MaxRampUpStep,
				AdjustmentLowerBound: -types.MaxRampDownStep,
				DeadbandLowerPct:     0.8,
				DeadbandUpperPct:     0.05,
			},
			consts.MetricCPUCPIContainer: {
				Kpp:                  0.1,
				Kpn:                  0.01,
				Kdp:                  0.0,
				Kdn:                  0.0,
				AdjustmentUpperBound: types.MaxRampUpStep,
				AdjustmentLowerBound: -types.MaxRampDownStep,
				DeadbandLowerPct:     0.95,
				DeadbandUpperPct:     0.02,
			},
			consts.MetricMemBandwidthNuma: {
				Kpp:                  0.1,
				Kpn:                  0.01,
				Kdp:                  0.0,
				Kdn:                  0.0,
				AdjustmentUpperBound: types.MaxRampUpStep,
				AdjustmentLowerBound: -types.MaxRampDownStep,
				DeadbandLowerPct:     0.95,
				DeadbandUpperPct:     0.02,
			},
		},
	}

	conf.GetDynamicConfiguration().EnableReclaim = true

	return conf
}

func newTestPolicyCanonical(t *testing.T, checkpointDir string, stateFileDir string,
	checkpointManagerDir string, regionInfo types.RegionInfo, metricFetcher metrictypes.MetricsFetcher, podSet types.PodSet,
) ProvisionPolicy {
	conf := generateCanonicalTestConfiguration(t, checkpointDir, stateFileDir, checkpointManagerDir)

	metaCacheTmp, err := metacache.NewMetaCacheImp(conf, metricspool.DummyMetricsEmitterPool{}, metricFetcher)
	require.NoError(t, err)
	require.NotNil(t, metaCacheTmp)

	genericCtx, err := katalyst_base.GenerateFakeGenericContext([]runtime.Object{})
	require.NoError(t, err)

	metaServerTmp, err := metaserver.NewMetaServer(genericCtx.Client, metrics.DummyMetrics{}, conf)
	assert.NoError(t, err)
	require.NotNil(t, metaServerTmp)

	p := NewPolicyCanonical(regionInfo.RegionName, regionInfo.RegionType, regionInfo.OwnerPoolName,
		conf, nil, metaCacheTmp, metaServerTmp, metrics.DummyMetrics{})
	err = metaCacheTmp.SetRegionInfo(regionInfo.RegionName, &regionInfo)
	assert.NoError(t, err)

	p.SetBindingNumas(regionInfo.BindingNumas, false)
	p.SetPodSet(podSet)

	return p
}

func constructPodFetcherCanonical(names []string) pod.PodFetcher {
	var pods []*v1.Pod
	for _, name := range names {
		pods = append(pods, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				UID:  k8types.UID(name),
			},
		})
	}

	return &pod.PodFetcherStub{PodList: pods}
}

func TestPolicyCanonical(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		regionInfo          types.RegionInfo
		containerInfo       map[string]map[string]types.ContainerInfo
		containerMetricData map[string]map[string]map[string]metricutil.MetricData
		resourceEssentials  types.ResourceEssentials
		controlEssentials   types.ControlEssentials
		wantResult          types.ControlKnob
	}{
		{
			name: "share_ramp_up",
			containerInfo: map[string]map[string]types.ContainerInfo{
				"pod0": {
					"container0": types.ContainerInfo{
						PodUID:        "pod0",
						PodName:       "pod0",
						ContainerName: "container0",
						QoSLevel:      apiconsts.PodAnnotationQoSLevelSharedCores,
						CPURequest:    4.0,
						RampUp:        true,
					},
				},
			},
			containerMetricData: map[string]map[string]map[string]metricutil.MetricData{
				"pod0": {
					"container0": {
						consts.MetricCPUUsageContainer: metricutil.MetricData{
							Value: 2,
						},
					},
				},
			},
			regionInfo: types.RegionInfo{
				RegionName: "share-xxx",
				RegionType: v1alpha1.QoSRegionTypeShare,
			},
			resourceEssentials: types.ResourceEssentials{
				EnableReclaim:       true,
				ResourceUpperBound:  90,
				ResourceLowerBound:  4,
				ReservedForAllocate: 0,
			},
			controlEssentials: types.ControlEssentials{
				ControlKnobs: types.ControlKnob{
					v1alpha1.ControlKnobNonReclaimedCPURequirement: {
						Value:  40,
						Action: types.ControlKnobActionNone,
					},
				},
				Indicators: types.Indicator{
					consts.MetricCPUSchedwait: {
						Current: 800,
						Target:  400,
					},
				},
				ReclaimOverlap: false,
			},
			wantResult: types.ControlKnob{
				v1alpha1.ControlKnobNonReclaimedCPURequirement: {
					Value:  4,
					Action: types.ControlKnobActionNone,
				},
			},
		},
		{
			name: "share_ramp_down",
			containerInfo: map[string]map[string]types.ContainerInfo{
				"pod0": {
					"container0": types.ContainerInfo{
						PodUID:        "pod0",
						PodName:       "pod0",
						ContainerName: "container0",
						QoSLevel:      apiconsts.PodAnnotationQoSLevelSharedCores,
						CPURequest:    4.0,
						RampUp:        false,
					},
				},
			},
			containerMetricData: map[string]map[string]map[string]metricutil.MetricData{
				"pod0": {
					"container0": {
						consts.MetricCPUUsageContainer: metricutil.MetricData{
							Value: 2,
						},
					},
				},
			},
			regionInfo: types.RegionInfo{
				RegionName: "share-xxx",
				RegionType: v1alpha1.QoSRegionTypeShare,
			},
			resourceEssentials: types.ResourceEssentials{
				EnableReclaim:       true,
				ResourceUpperBound:  90,
				ResourceLowerBound:  4,
				ReservedForAllocate: 0,
			},
			controlEssentials: types.ControlEssentials{
				ControlKnobs: types.ControlKnob{
					v1alpha1.ControlKnobNonReclaimedCPURequirement: {
						Value:  40,
						Action: types.ControlKnobActionNone,
					},
				},
				Indicators: types.Indicator{
					consts.MetricCPUSchedwait: {
						Current: 4,
						Target:  400,
					},
				},
				ReclaimOverlap: false,
			},
			wantResult: types.ControlKnob{
				v1alpha1.ControlKnobNonReclaimedCPURequirement: {
					Value:  2.5,
					Action: types.ControlKnobActionNone,
				},
			},
		},
		{
			name: "dedicated_numa_exclusive",
			containerInfo: map[string]map[string]types.ContainerInfo{
				"pod0": {
					"container0": types.ContainerInfo{
						PodUID:        "pod0",
						PodName:       "pod0",
						ContainerName: "container0",
						QoSLevel:      apiconsts.PodAnnotationQoSLevelDedicatedCores,
						CPURequest:    4.0,
						RampUp:        false,
						TopologyAwareAssignments: map[int]machine.CPUSet{
							0: machine.NewCPUSet(0, 1, 2, 3),
						},
					},
				},
			},
			regionInfo: types.RegionInfo{
				RegionName:   "dedicated-numa-exclusive-xxx",
				RegionType:   v1alpha1.QoSRegionTypeDedicatedNumaExclusive,
				BindingNumas: machine.NewCPUSet(0),
			},
			resourceEssentials: types.ResourceEssentials{
				EnableReclaim:       true,
				ResourceUpperBound:  90,
				ResourceLowerBound:  4,
				ReservedForAllocate: 0,
			},
			controlEssentials: types.ControlEssentials{
				ControlKnobs: types.ControlKnob{
					v1alpha1.ControlKnobNonReclaimedCPURequirement: {
						Value:  40,
						Action: types.ControlKnobActionNone,
					},
				},
				Indicators: types.Indicator{
					consts.MetricCPUCPIContainer: {
						Current: 2.0,
						Target:  1.0,
					},
					consts.MetricMemBandwidthNuma: {
						Current: 4,
						Target:  40,
					},
				},
				ReclaimOverlap: false,
			},
			wantResult: types.ControlKnob{
				v1alpha1.ControlKnobNonReclaimedCPURequirement: {
					Value:  4,
					Action: types.ControlKnobActionNone,
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			checkpointDir, err := os.MkdirTemp("", "checkpoint")
			require.NoError(t, err)
			defer func() { _ = os.RemoveAll(checkpointDir) }()

			stateFileDir, err := os.MkdirTemp("", "statefile")
			require.NoError(t, err)
			defer func() { _ = os.RemoveAll(stateFileDir) }()

			checkpointManagerDir, err := os.MkdirTemp("", "checkpointmanager")
			require.NoError(t, err)
			defer func() { _ = os.RemoveAll(checkpointManagerDir) }()

			podSet := make(types.PodSet)
			for podUID, info := range tt.containerInfo {
				for containerName := range info {
					podSet.Insert(podUID, containerName)
				}
			}

			metricFetcher := metric.NewFakeMetricsFetcher(metrics.DummyMetrics{})
			for podUID, m := range tt.containerMetricData {
				for containerName, c := range m {
					for name, d := range c {
						metricFetcher.(*metric.FakeMetricsFetcher).SetContainerMetric(podUID, containerName, name, d)
					}
				}
			}

			policy := newTestPolicyCanonical(t, checkpointDir, stateFileDir,
				checkpointManagerDir, tt.regionInfo, metricFetcher, podSet).(*PolicyCanonical)
			assert.NotNil(t, policy)

			podNames := []string{}
			for podName, containerSet := range tt.containerInfo {
				podNames = append(podNames, podName)
				for containerName, info := range containerSet {
					err = policy.metaReader.(*metacache.MetaCacheImp).AddContainer(podName, containerName, &info)
					assert.Nil(t, err)
				}
			}
			policy.metaServer.MetaAgent.SetPodFetcher(constructPodFetcherCanonical(podNames))

			policy.SetEssentials(tt.resourceEssentials, tt.controlEssentials)
			_ = policy.Update()
			controlKnobUpdated, err := policy.GetControlKnobAdjusted()

			assert.NoError(t, err)
			assert.Equal(t, tt.wantResult, controlKnobUpdated)
		})
	}
}

func TestInvalidPolicyCanonical(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		regionInfo          types.RegionInfo
		containerInfo       map[string]map[string]types.ContainerInfo
		containerMetricData map[string]map[string]map[string]metricutil.MetricData
		resourceEssentials  types.ResourceEssentials
		controlEssentials   types.ControlEssentials
		wantResult          types.ControlKnob
	}{
		{
			name: "InvalidControlKnobNonReclaimedCPURequirement",
			containerInfo: map[string]map[string]types.ContainerInfo{
				"pod0": {
					"container0": types.ContainerInfo{
						PodUID:        "pod0",
						PodName:       "pod0",
						ContainerName: "container0",
						QoSLevel:      apiconsts.PodAnnotationQoSLevelSharedCores,
						CPURequest:    4.0,
						RampUp:        true,
					},
				},
			},
			containerMetricData: map[string]map[string]map[string]metricutil.MetricData{
				"pod0": {
					"container0": {
						consts.MetricCPUUsageContainer: metricutil.MetricData{
							Value: 2,
						},
					},
				},
			},
			regionInfo: types.RegionInfo{
				RegionName: "share-xxx",
				RegionType: v1alpha1.QoSRegionTypeShare,
			},
			resourceEssentials: types.ResourceEssentials{
				EnableReclaim:       true,
				ResourceUpperBound:  90,
				ResourceLowerBound:  4,
				ReservedForAllocate: 0,
			},
			controlEssentials: types.ControlEssentials{
				ControlKnobs: types.ControlKnob{
					v1alpha1.ControlKnobNonReclaimedCPURequirement: {
						Value:  -1,
						Action: types.ControlKnobActionNone,
					},
				},
				Indicators: types.Indicator{
					consts.MetricCPUSchedwait: {
						Current: 800,
						Target:  400,
					},
				},
				ReclaimOverlap: false,
			},
			wantResult: types.ControlKnob{
				v1alpha1.ControlKnobNonReclaimedCPURequirement: {
					Value:  4,
					Action: types.ControlKnobActionNone,
				},
			},
		},
		{
			name: "InvalidControlKnobReclaimedCoresCPUQuota",
			containerInfo: map[string]map[string]types.ContainerInfo{
				"pod0": {
					"container0": types.ContainerInfo{
						PodUID:        "pod0",
						PodName:       "pod0",
						ContainerName: "container0",
						QoSLevel:      apiconsts.PodAnnotationQoSLevelSharedCores,
						CPURequest:    4.0,
						RampUp:        false,
					},
				},
			},
			containerMetricData: map[string]map[string]map[string]metricutil.MetricData{
				"pod0": {
					"container0": {
						consts.MetricCPUUsageContainer: metricutil.MetricData{
							Value: 2,
						},
					},
				},
			},
			regionInfo: types.RegionInfo{
				RegionName: "share-xxx",
				RegionType: v1alpha1.QoSRegionTypeShare,
			},
			resourceEssentials: types.ResourceEssentials{
				EnableReclaim:       true,
				ResourceUpperBound:  90,
				ResourceLowerBound:  4,
				ReservedForAllocate: 0,
			},
			controlEssentials: types.ControlEssentials{
				ControlKnobs: types.ControlKnob{
					v1alpha1.ControlKnobReclaimedCoresCPUQuota: {
						Value:  -1,
						Action: types.ControlKnobActionNone,
					},
				},
				Indicators: types.Indicator{
					consts.MetricCPUSchedwait: {
						Current: 4,
						Target:  400,
					},
				},
				ReclaimOverlap: false,
			},
			wantResult: types.ControlKnob{
				v1alpha1.ControlKnobNonReclaimedCPURequirement: {
					Value:  2.5,
					Action: types.ControlKnobActionNone,
				},
			},
		},
		{
			name: "MissingControlKnob",
			containerInfo: map[string]map[string]types.ContainerInfo{
				"pod0": {
					"container0": types.ContainerInfo{
						PodUID:        "pod0",
						PodName:       "pod0",
						ContainerName: "container0",
						QoSLevel:      apiconsts.PodAnnotationQoSLevelDedicatedCores,
						CPURequest:    4.0,
						RampUp:        false,
						TopologyAwareAssignments: map[int]machine.CPUSet{
							0: machine.NewCPUSet(0, 1, 2, 3),
						},
					},
				},
			},
			regionInfo: types.RegionInfo{
				RegionName:   "dedicated-numa-exclusive-xxx",
				RegionType:   v1alpha1.QoSRegionTypeDedicatedNumaExclusive,
				BindingNumas: machine.NewCPUSet(0),
			},
			resourceEssentials: types.ResourceEssentials{
				EnableReclaim:       true,
				ResourceUpperBound:  90,
				ResourceLowerBound:  4,
				ReservedForAllocate: 0,
			},
			controlEssentials: types.ControlEssentials{
				Indicators: types.Indicator{
					consts.MetricCPUCPIContainer: {
						Current: 2.0,
						Target:  1.0,
					},
					consts.MetricMemBandwidthNuma: {
						Current: 4,
						Target:  40,
					},
				},
				ReclaimOverlap: false,
			},
			wantResult: types.ControlKnob{
				v1alpha1.ControlKnobNonReclaimedCPURequirement: {
					Value:  4,
					Action: types.ControlKnobActionNone,
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			checkpointDir, err := os.MkdirTemp("", "checkpoint")
			require.NoError(t, err)
			defer func() { _ = os.RemoveAll(checkpointDir) }()

			stateFileDir, err := os.MkdirTemp("", "statefile")
			require.NoError(t, err)
			defer func() { _ = os.RemoveAll(stateFileDir) }()

			checkpointManagerDir, err := os.MkdirTemp("", "checkpointmanager")
			require.NoError(t, err)
			defer func() { _ = os.RemoveAll(checkpointManagerDir) }()

			podSet := make(types.PodSet)
			for podUID, info := range tt.containerInfo {
				for containerName := range info {
					podSet.Insert(podUID, containerName)
				}
			}

			metricFetcher := metric.NewFakeMetricsFetcher(metrics.DummyMetrics{})
			for podUID, m := range tt.containerMetricData {
				for containerName, c := range m {
					for name, d := range c {
						metricFetcher.(*metric.FakeMetricsFetcher).SetContainerMetric(podUID, containerName, name, d)
					}
				}
			}

			policy := newTestPolicyCanonical(t, checkpointDir, stateFileDir,
				checkpointManagerDir, tt.regionInfo, metricFetcher, podSet).(*PolicyCanonical)
			assert.NotNil(t, policy)

			podNames := []string{}
			for podName, containerSet := range tt.containerInfo {
				podNames = append(podNames, podName)
				for containerName, info := range containerSet {
					err = policy.metaReader.(*metacache.MetaCacheImp).AddContainer(podName, containerName, &info)
					assert.Nil(t, err)
				}
			}
			policy.metaServer.MetaAgent.SetPodFetcher(constructPodFetcherCanonical(podNames))

			policy.SetEssentials(tt.resourceEssentials, tt.controlEssentials)
			err = policy.Update()

			assert.Error(t, err)
		})
	}
}
