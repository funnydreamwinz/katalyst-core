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

package provision

import (
	"github.com/kubewharf/katalyst-api/pkg/apis/workload/v1alpha1"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/types"
)

type PolicyRamaConfiguration struct {
	PIDParameters                   map[string]types.FirstOrderPIDParams
	EnableBorwein                   bool
	EnableBorweinModelResultFetcher bool
}

func NewPolicyRamaConfiguration() *PolicyRamaConfiguration {
	return &PolicyRamaConfiguration{
		PIDParameters: map[string]types.FirstOrderPIDParams{
			string(v1alpha1.ServiceSystemIndicatorNameCPUSchedWait): {
				Kpp:                  0.05,
				Kpn:                  0.009,
				Kdp:                  0.0,
				Kdn:                  0.0,
				AdjustmentUpperBound: types.MaxRampUpStep,
				AdjustmentLowerBound: -types.MaxRampDownStep,
				DeadbandUpperPct:     0.05,
				DeadbandLowerPct:     0.2,
			},
			string(v1alpha1.ServiceSystemIndicatorNameCPUUsageRatio): {
				Kpp:                  0.1,
				Kpn:                  0.04,
				Kdp:                  0.0,
				Kdn:                  0.0,
				AdjustmentUpperBound: types.MaxRampUpStep,
				AdjustmentLowerBound: -types.MaxRampDownStep,
				DeadbandUpperPct:     0.01,
				DeadbandLowerPct:     0.06,
			},
			string(v1alpha1.ServiceSystemIndicatorNameCPI): {
				Kpp:                  0.1,
				Kpn:                  0.04,
				Kdp:                  0.0,
				Kdn:                  0.0,
				AdjustmentUpperBound: types.MaxRampUpStep,
				AdjustmentLowerBound: -types.MaxRampDownStep,
				DeadbandUpperPct:     0.0,
				DeadbandLowerPct:     0.02,
			},
			string(v1alpha1.ServiceSystemIndicatorNameMemoryAccessReadLatency): {
				Kpp:                  0.05,
				Kpn:                  0.009,
				Kdp:                  0.0,
				Kdn:                  0.0,
				AdjustmentUpperBound: types.MaxRampUpStep,
				AdjustmentLowerBound: -types.MaxRampDownStep,
				DeadbandUpperPct:     0.05,
				DeadbandLowerPct:     0.2,
			},
			string(v1alpha1.ServiceSystemIndicatorNameMemoryAccessWriteLatency): {
				Kpp:                  0.05,
				Kpn:                  0.009,
				Kdp:                  0.0,
				Kdn:                  0.0,
				AdjustmentUpperBound: types.MaxRampUpStep,
				AdjustmentLowerBound: -types.MaxRampDownStep,
				DeadbandUpperPct:     0.05,
				DeadbandLowerPct:     0.2,
			},
			string(v1alpha1.ServiceSystemIndicatorNameMemoryL3MissLatency): {
				Kpp:                  0.05,
				Kpn:                  0.009,
				Kdp:                  0.0,
				Kdn:                  0.0,
				AdjustmentUpperBound: types.MaxRampUpStep,
				AdjustmentLowerBound: -types.MaxRampDownStep,
				DeadbandUpperPct:     0.05,
				DeadbandLowerPct:     0.2,
			},
		},
	}
}
