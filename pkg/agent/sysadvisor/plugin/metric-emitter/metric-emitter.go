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

package metric_emitter

import (
	"context"

	"k8s.io/klog/v2"

	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/metacache"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/metric-emitter/external"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/metric-emitter/node"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/metric-emitter/pod"
	"github.com/kubewharf/katalyst-core/pkg/config"
	"github.com/kubewharf/katalyst-core/pkg/metaserver"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	metricspool "github.com/kubewharf/katalyst-core/pkg/metrics/metrics-pool"
)

// CustomMetricSyncer is used as a common implementation for custom-metrics emitter
type CustomMetricSyncer interface {
	Name() string
	Run(ctx context.Context)
}

const PluginNameCustomMetricEmitter = "metric-emitter-plugin"

type CustomMetricEmitter struct {
	syncers []CustomMetricSyncer
}

func NewCustomMetricEmitter(conf *config.Configuration, _ interface{}, emitterPool metricspool.MetricsEmitterPool,
	metaServer *metaserver.MetaServer, metaCache *metacache.MetaCache) (plugin.SysAdvisorPlugin, error) {
	dataEmitter, err := emitterPool.GetMetricsEmitter(metricspool.PrometheusMetricOptions{
		Path: metrics.PrometheusMetricPathNameCustomMetric,
	})
	if err != nil {
		klog.Errorf("[cus-metric-emitter] failed to init metric emitter: %v", err)
		return plugin.DummySysAdvisorPlugin{}, err
	}
	metricEmitter := emitterPool.GetDefaultMetricsEmitter().WithTags("custom-metric")

	return &CustomMetricEmitter{
		syncers: []CustomMetricSyncer{
			external.NewMetricSyncerExternal(conf, metricEmitter, dataEmitter, metaServer, metaCache),
			node.NewMetricSyncerNode(conf, metricEmitter, dataEmitter, metaServer, metaCache),
			pod.NewMetricSyncerPod(conf, metricEmitter, dataEmitter, metaServer, metaCache),
		},
	}, nil
}

func (cme *CustomMetricEmitter) Name() string {
	return PluginNameCustomMetricEmitter
}

func (cme *CustomMetricEmitter) Init() error {
	return nil
}

// Run is the main logic to get metric data from multiple sources
// and emit through the standard emit interface.
func (cme *CustomMetricEmitter) Run(ctx context.Context) {
	klog.Info("custom metrics emitter stated")

	for _, syncer := range cme.syncers {
		syncer.Run(ctx)
	}
	<-ctx.Done()
}
