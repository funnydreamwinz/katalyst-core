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

package config

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	"github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	"github.com/kubewharf/katalyst-core/pkg/client"
	pkgconfig "github.com/kubewharf/katalyst-core/pkg/config"
	"github.com/kubewharf/katalyst-core/pkg/config/dynamic"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	"github.com/kubewharf/katalyst-core/pkg/util/native"
	"github.com/kubewharf/katalyst-core/pkg/util/syntax"
)

const (
	updateConfigInterval     = 3 * time.Second
	updateConfigJitterFactor = 0.5
)

const (
	metricsNameUpdateConfig   = "metaserver_update_config"
	metricsNameLoadCheckpoint = "metaserver_load_checkpoint"

	metricsValueStatusCheckpointNotFoundOrCorrupted = "notFoundOrCorrupted"
	metricsValueStatusCheckpointInvalidOrExpired    = "invalidOrExpired"
	metricsValueStatusCheckpointSuccess             = "success"
)

const (
	configManagerCheckpoint = "config_manager_checkpoint"
)

var (
	katalystConfigGVRToGVKMap = getGVRToGVKMap()

	updateConfigBackoff = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2,
		Jitter:   0.1,
		Steps:    5,
		Cap:      15 * time.Second,
	}
)

// ConfigurationRegister indicates that each user should pass an ConfigurationRegister function
// to ConfigManager; every time when user-interested configuration changes, the ConfigManager
// should call the ConfigurationRegister function to notify this change event.
type ConfigurationRegister interface {
	ApplyConfig(*pkgconfig.DynamicConfiguration)
}

type DummyConfigurationRegister struct{}

func (d *DummyConfigurationRegister) ApplyConfig(*pkgconfig.DynamicConfiguration) {}

// ConfigurationManager is a user for ConfigurationLoader working for dynamic configuration manager
type ConfigurationManager interface {
	// UpdateConfig trigger dynamic configuration update directly
	UpdateConfig(ctx context.Context) error
	// AddConfigWatcher add gvr to list which will be watched to get dynamic configuration
	AddConfigWatcher(gvrs ...metav1.GroupVersionResource) error
	// Register is used when a user wants to get dynamic configuration
	Register(registers ...ConfigurationRegister)
	// Run starts the main loop
	Run(ctx context.Context)
}

// DynamicConfigManager is to fetch dynamic config from remote
type DynamicConfigManager struct {
	// defaultConfig is used to store the static configuration parsed from flags
	// currentConfig merges default conf with dynamic conf (defined in kcc); and
	// the dynamic conf is used as an incremental way.
	defaultConfig *pkgconfig.DynamicConfiguration
	currentConfig *pkgconfig.DynamicConfiguration

	configLoader ConfigurationLoader
	emitter      metrics.MetricEmitter

	// resourceGVRMap records those GVR that should be interested
	// gvrToKind maps from GVR to GVK (only kind can be used to reflect objects)
	mux            sync.RWMutex
	resourceGVRMap map[string]metav1.GroupVersionResource

	// todo: it would better if each component will be triggered only if its
	//  interested configurations are changed; and registerList should be changed to registerMap
	registerList []ConfigurationRegister

	// checkpoint stores recent dynamic config
	checkpointManager   checkpointmanager.CheckpointManager
	checkpointGraceTime time.Duration
}

// NewDynamicConfigManager new a dynamic config manager use katalyst custom config sdk.
func NewDynamicConfigManager(clientSet *client.GenericClientSet, emitter metrics.MetricEmitter, conf *pkgconfig.Configuration) (ConfigurationManager, error) {
	configLoader := NewKatalystCustomConfigLoader(clientSet, conf.ConfigTTL, conf.NodeName)

	checkpointManager, err := checkpointmanager.NewCheckpointManager(conf.CheckpointManagerDir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize checkpoint manager: %v", err)
	}

	return &DynamicConfigManager{
		defaultConfig:       conf.DynamicConfiguration,
		currentConfig:       deepCopy(conf.DynamicConfiguration),
		configLoader:        configLoader,
		emitter:             emitter,
		resourceGVRMap:      make(map[string]metav1.GroupVersionResource),
		checkpointManager:   checkpointManager,
		checkpointGraceTime: conf.ConfigCheckpointGraceTime,
	}, nil
}

// AddConfigWatcher add gvr to list which will be watched to get dynamic configuration
func (c *DynamicConfigManager) AddConfigWatcher(gvrs ...metav1.GroupVersionResource) error {
	c.mux.Lock()
	defer c.mux.Unlock()

	for _, gvr := range gvrs {
		if oldGVR, ok := c.resourceGVRMap[gvr.Resource]; ok && gvr != oldGVR {
			return fmt.Errorf("resource %s already reggistered by gvrs %s which is different with %s",
				gvr.Resource, oldGVR.String(), gvr.String())

		}

		c.resourceGVRMap[gvr.Resource] = gvr
	}

	return nil
}

// Register is to add configurationRegister to a list which will be applied
func (c *DynamicConfigManager) Register(register ...ConfigurationRegister) {
	c.mux.Lock()
	defer c.mux.Unlock()

	c.registerList = append(c.registerList, register...)
}

// Run is to start update config loops until the context is done
func (c *DynamicConfigManager) Run(ctx context.Context) {
	go wait.JitterUntilWithContext(ctx, func(context.Context) {
		if err := c.tryUpdateConfig(ctx, true); err != nil {
			klog.Errorf("try update config error: %v", err)
		}
	}, updateConfigInterval, updateConfigJitterFactor, true)
	<-ctx.Done()
}

// UpdateConfig is to update dynamic config directly
func (c *DynamicConfigManager) UpdateConfig(ctx context.Context) error {
	err := wait.ExponentialBackoff(updateConfigBackoff, func() (bool, error) {
		err := c.tryUpdateConfig(ctx, false)
		if err == nil {
			return true, nil
		}
		return false, nil
	})
	return err
}

func (c *DynamicConfigManager) tryUpdateConfig(ctx context.Context, skipError bool) error {
	c.mux.RLock()
	defer c.mux.RUnlock()

	config, updated, err := c.getConfig(ctx)
	if !skipError && err != nil {
		_ = c.emitter.StoreInt64(metricsNameUpdateConfig, 1, metrics.MetricTypeNameCount, metrics.MetricTag{
			Key: "status", Val: "failed",
		})
		return err
	}

	_ = c.emitter.StoreInt64(metricsNameUpdateConfig, 1, metrics.MetricTypeNameCount, metrics.MetricTag{
		Key: "status", Val: "success",
	})

	if !updated {
		return nil
	}

	for _, register := range c.registerList {
		register.ApplyConfig(config)
	}
	return nil
}

// getConfig is used to get dynamic agent config from remote
func (c *DynamicConfigManager) getConfig(ctx context.Context) (*pkgconfig.DynamicConfiguration, bool, error) {
	dynamicConfiguration, success, err := c.updateDynamicConfig(c.resourceGVRMap, katalystConfigGVRToGVKMap,
		func(gvr metav1.GroupVersionResource, conf interface{}) error {
			return c.configLoader.LoadConfig(ctx, gvr, conf)
		},
	)
	if !success {
		return c.currentConfig, false, err
	}

	newConfig, updated := applyDynamicConfig(c.defaultConfig, c.currentConfig, dynamicConfiguration)
	if updated {
		c.currentConfig = newConfig
	}

	return newConfig, updated, err
}

func (c *DynamicConfigManager) writeCheckpoint(kind string, configData reflect.Value) {
	// read checkpoint to get config data related to other gvr
	data, err := c.readCheckpoint()
	if err != nil {
		klog.Errorf("load checkpoint from %q failed: %v, try to overwrite it", configManagerCheckpoint, err)
		_ = c.emitter.StoreInt64(metricsNameLoadCheckpoint, 1, metrics.MetricTypeNameCount, []metrics.MetricTag{
			{Key: "status", Val: metricsValueStatusCheckpointNotFoundOrCorrupted},
			{Key: "kind", Val: kind},
		}...)
	}

	// checkpoint doesn't exist or became corrupted, make a new checkpoint
	if data == nil {
		data = NewCheckpoint(make(map[string]TargetConfigData))
	}

	// set config value and timestamp for kind
	data.SetData(kind, configData, metav1.Now())
	err = c.checkpointManager.CreateCheckpoint(configManagerCheckpoint, data)
	if err != nil {
		klog.Errorf("failed to write checkpoint file %q: %v", configManagerCheckpoint, err)
	}
}

func (c *DynamicConfigManager) readCheckpoint() (ConfigManagerCheckpoint, error) {
	configResponses := make(map[string]TargetConfigData)
	cp := NewCheckpoint(configResponses)
	err := c.checkpointManager.GetCheckpoint(configManagerCheckpoint, cp)
	if err != nil {
		return nil, err
	}

	return cp, nil
}

func (c *DynamicConfigManager) updateDynamicConfig(resourceGVRMap map[string]metav1.GroupVersionResource,
	gvrToKind map[schema.GroupVersionResource]schema.GroupVersionKind,
	loader func(gvr metav1.GroupVersionResource, conf interface{}) error) (*dynamic.DynamicConfigCRD, bool, error) {
	dynamicConfiguration := &dynamic.DynamicConfigCRD{}
	success := false

	var errList []error
	for _, gvr := range resourceGVRMap {
		schemaGVR := native.ToSchemaGVR(gvr.Group, gvr.Version, gvr.Resource)
		kind, ok := gvrToKind[schemaGVR]
		if !ok {
			errList = append(errList, fmt.Errorf("gvk of gvr %s is not found", gvr))
			continue
		}

		// get target dynamic config configField by kind
		configField := reflect.ValueOf(dynamicConfiguration).Elem().FieldByName(kind.Kind)

		// create a new instance of this configField type
		newConfigData := reflect.New(configField.Type().Elem())
		err := loader(gvr, newConfigData.Interface())
		if err != nil {
			klog.Warningf("failed to load targetConfigMeta from targetConfigMeta fetcher: %s", err)
			// get target dynamic configField value from checkpoint
			data, err := c.readCheckpoint()
			if err != nil {
				_ = c.emitter.StoreInt64(metricsNameLoadCheckpoint, 1, metrics.MetricTypeNameRaw, []metrics.MetricTag{
					{Key: "status", Val: metricsValueStatusCheckpointNotFoundOrCorrupted},
					{Key: "kind", Val: kind.Kind},
				}...)
				errList = append(errList, fmt.Errorf("failed to get targetConfigMeta from checkpoint"))
				continue
			} else {
				configData, timestamp := data.GetData(kind.Kind)
				if configData.Kind() == reflect.Ptr && !configData.IsNil() &&
					time.Now().Before(timestamp.Add(c.checkpointGraceTime)) {
					newConfigData = configData
					klog.Infof("failed to load targetConfigMeta from remote, use local checkpoint instead")
					_ = c.emitter.StoreInt64(metricsNameLoadCheckpoint, 1, metrics.MetricTypeNameRaw, []metrics.MetricTag{
						{Key: "status", Val: metricsValueStatusCheckpointSuccess},
						{Key: "kind", Val: kind.Kind},
					}...)
				} else {
					_ = c.emitter.StoreInt64(metricsNameLoadCheckpoint, 1, metrics.MetricTypeNameRaw, []metrics.MetricTag{
						{Key: "status", Val: metricsValueStatusCheckpointInvalidOrExpired},
						{Key: "kind", Val: kind.Kind},
					}...)
					errList = append(errList, fmt.Errorf("checkpoint data for gvr %v is empty or out of date", gvr.String()))
					continue
				}
			}
		}

		// set target dynamic configField by new config field
		configField.Set(newConfigData)
		success = true
		c.writeCheckpoint(kind.Kind, newConfigData)
	}

	return dynamicConfiguration, success, errors.NewAggregate(errList)
}

func getGVRToGVKMap() map[schema.GroupVersionResource]schema.GroupVersionKind {
	scheme := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	knownTypes := scheme.AllKnownTypes()
	gvrToKind := make(map[schema.GroupVersionResource]schema.GroupVersionKind)
	for kind := range knownTypes {
		plural, singular := meta.UnsafeGuessKindToResource(kind)
		gvrToKind[plural] = kind
		gvrToKind[singular] = kind
	}
	return gvrToKind
}

func applyDynamicConfig(defaultConfig, currentConfig *pkgconfig.DynamicConfiguration,
	dynamicConf *dynamic.DynamicConfigCRD) (*pkgconfig.DynamicConfiguration, bool) {
	// copy default config from env and apply remote dynamic config to it
	newConfig := deepCopy(defaultConfig)
	newConfig.ApplyAgentConfiguration(dynamicConf)

	if apiequality.Semantic.DeepEqual(newConfig, currentConfig) {
		return currentConfig, false
	}

	return newConfig, true
}

func deepCopy(src *pkgconfig.DynamicConfiguration) *pkgconfig.DynamicConfiguration {
	return syntax.DeepCopy(src).(*pkgconfig.DynamicConfiguration)
}
