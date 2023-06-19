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

package helper

import (
	"math"

	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/types"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
)

type PIDController struct {
	params          types.FirstOrderPIDParams
	adjustmentTotal float64
	controlKnobPrev float64
	errorValue      float64
	errorValuePrev  float64
	errorRate       float64
}

func NewPIDController(params types.FirstOrderPIDParams) PIDController {
	return PIDController{
		params:          params,
		adjustmentTotal: 0,
		controlKnobPrev: 0,
		errorValue:      0,
		errorValuePrev:  0,
		errorRate:       0,
	}
}

func (c *PIDController) Adjust(controlKnob, target, current float64) float64 {
	var (
		kp, kd, kpSign, kdSign float64
	)

	c.errorValuePrev = c.errorValue
	c.errorValue = math.Log(current) - math.Log(target)
	c.errorRate = math.Abs(c.errorValue) - math.Abs(c.errorValuePrev)

	if c.errorValue >= 0 {
		kp = c.params.Kpp
		kpSign = 1
	} else {
		kp = c.params.Kpn
		kpSign = -1
	}

	if c.errorRate >= 0 {
		kdSign = kpSign
	} else {
		kdSign = -kpSign
	}

	if kdSign >= 0 {
		kd = c.params.Kdp
	} else {
		kd = c.params.Kdn
	}

	pterm := kp * c.errorValue
	dterm := kdSign * kd * math.Abs(c.errorRate)
	adjustment := pterm + dterm

	if c.controlKnobPrev != controlKnob {
		c.adjustmentTotal = 0
	}
	c.adjustmentTotal += adjustment
	c.adjustmentTotal = general.Clamp(c.adjustmentTotal, c.params.AdjustmentLowerBound, c.params.AdjustmentUpperBound)

	general.Infof("adjustment %.2f adjustmentTotal %.2f target %.2f current %.2f errorValue %.2f errorRate %.2f pterm %.2f dterm %.2f",
		adjustment, c.adjustmentTotal, target, current, c.errorValue, c.errorRate, pterm, dterm)

	return controlKnob + c.adjustmentTotal
}