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

package poweraware

import (
	"context"
	"testing"

	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/poweraware/controller"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/poweraware/evictor"
	"github.com/kubewharf/katalyst-core/pkg/config"
	agentconf "github.com/kubewharf/katalyst-core/pkg/config/agent"
	"github.com/kubewharf/katalyst-core/pkg/config/agent/sysadvisor"
	"github.com/kubewharf/katalyst-core/pkg/config/agent/sysadvisor/poweraware"
	"github.com/kubewharf/katalyst-core/pkg/config/generic"
	"github.com/kubewharf/katalyst-core/pkg/metaserver"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent/node"
	metricspool "github.com/kubewharf/katalyst-core/pkg/metrics/metrics-pool"
)

type stubNodeFetcher struct {
	node.NodeFetcher
}

func Test_powerAwarePlugin_Name(t *testing.T) {
	t.Parallel()

	expectedName := "test"
	expectedDryRun := true
	expectedDisabled := false

	stubMetaServer := &metaserver.MetaServer{
		MetaAgent: &agent.MetaAgent{
			NodeFetcher: &stubNodeFetcher{},
		},
	}
	dummyPluginConf := poweraware.PowerAwarePluginOptions{
		Disabled: expectedDisabled,
		DryRun:   expectedDryRun,
	}
	dummyEmitterPool := metricspool.DummyMetricsEmitterPool{}
	dummyController := controller.NewController(
		expectedDryRun,
		evictor.NewNoopPodEvictor(),
		dummyEmitterPool.GetDefaultMetricsEmitter(),
		stubMetaServer.NodeFetcher,
		nil,
		stubMetaServer.PodFetcher,
		nil,
		nil)

	p, err := newPluginWithController(expectedName,
		&config.Configuration{
			AgentConfiguration: &agentconf.AgentConfiguration{
				StaticAgentConfiguration: &agentconf.StaticAgentConfiguration{
					SysAdvisorPluginsConfiguration: &sysadvisor.SysAdvisorPluginsConfiguration{
						PowerAwarePluginOptions: &dummyPluginConf,
					},
				},
			},
			GenericConfiguration: &generic.GenericConfiguration{
				QoSConfiguration: generic.NewQoSConfiguration(),
			},
		},
		dummyController,
	)
	if err != nil {
		t.Errorf("unexpected error: %#v", err)
	}

	if p.Name() != expectedName {
		t.Errorf("expected %s, got %s", expectedName, p.Name())
	}

	pap := p.(*powerAwarePlugin)
	if pap.dryRun != expectedDryRun {
		t.Errorf("expected dryrun %v, got %v", expectedDryRun, pap.dryRun)
	}
	if pap.disabled != expectedDisabled {
		t.Errorf("expected disabled %v, got %v", expectedDisabled, pap.disabled)
	}
}

func Test_powerAwarePlugin_Init(t *testing.T) {
	t.Parallel()
	dummyEmitter := metricspool.DummyMetricsEmitterPool{}.GetDefaultMetricsEmitter().WithTags("advisor-poweraware")

	type fields struct {
		name        string
		disabled    bool
		dryRun      bool
		nodeFetcher node.NodeFetcher
	}
	tests := []struct {
		name    string
		fields  fields
		wantErr bool
	}{
		{
			name: "happy path no error",
			fields: fields{
				name:     "dummy",
				disabled: false,
			},
			wantErr: false,
		},
		{
			name: "disabled path returns error",
			fields: fields{
				name:     "dummy",
				disabled: true,
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := powerAwarePlugin{
				name:     tt.fields.name,
				disabled: tt.fields.disabled,
				dryRun:   tt.fields.dryRun,
				controller: controller.NewController(false, evictor.NewNoopPodEvictor(), dummyEmitter,
					tt.fields.nodeFetcher, nil, nil, nil, nil,
				),
			}
			if err := p.Init(); (err != nil) != tt.wantErr {
				t.Errorf("Init() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

type dummyController struct {
	controller.PowerAwareController
	called bool
}

func (d *dummyController) Run(ctx context.Context) {
	d.called = true
}

func Test_powerAwarePlugin_Run(t *testing.T) {
	t.Parallel()
	dummyController := &dummyController{}
	type fields struct {
		name       string
		disabled   bool
		dryRun     bool
		controller controller.PowerAwareController
	}
	type args struct {
		ctx context.Context
	}
	tests := []struct {
		name   string
		fields fields
		args   args
	}{
		{
			name: "happy path calls controller Run",
			fields: fields{
				controller: dummyController,
			},
			args: args{
				ctx: context.TODO(),
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := powerAwarePlugin{
				name:       tt.fields.name,
				disabled:   tt.fields.disabled,
				dryRun:     tt.fields.dryRun,
				controller: tt.fields.controller,
			}
			p.Run(tt.args.ctx)
			if !dummyController.called {
				t.Error("expected controller Run called; but not")
			}
		})
	}
}
