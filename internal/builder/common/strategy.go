/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// BuildDeploymentStrategy renders a CR rollout strategy into its apps/v1 shape, returning
// nil for an unset spec so the Deployment keeps the apps/v1 default. An explicit
// RollingUpdate materializes the apps/v1 25% defaults so CreateOrUpdate does not repeatedly
// clear values that the API server restores. A zero maxSurge with no maxUnavailable renders
// maxUnavailable: 1.
func BuildDeploymentStrategy(s *otilmv1alpha1.DeploymentStrategySpec) *appsv1.DeploymentStrategy {
	if s == nil {
		return nil
	}

	strategy := &appsv1.DeploymentStrategy{Type: appsv1.DeploymentStrategyType(s.Type)}
	if strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		return strategy
	}

	defaultBound := intstr.FromString("25%")
	rollingUpdate := &appsv1.RollingUpdateDeployment{
		MaxSurge:       copyIntOrString(&defaultBound),
		MaxUnavailable: copyIntOrString(&defaultBound),
	}
	if s.RollingUpdate != nil {
		if s.RollingUpdate.MaxSurge != nil {
			rollingUpdate.MaxSurge = copyIntOrString(s.RollingUpdate.MaxSurge)
		}
		if s.RollingUpdate.MaxUnavailable != nil {
			rollingUpdate.MaxUnavailable = copyIntOrString(s.RollingUpdate.MaxUnavailable)
		} else if isZeroBound(rollingUpdate.MaxSurge) {
			one := intstr.FromInt32(1)
			rollingUpdate.MaxUnavailable = &one
		}
	}
	strategy.RollingUpdate = rollingUpdate

	return strategy
}

// copyIntOrString copies a bound so the rendered Deployment shares no memory with the CR.
func copyIntOrString(v *intstr.IntOrString) *intstr.IntOrString {
	if v == nil {
		return nil
	}
	copied := *v
	return &copied
}

// isZeroBound reports whether a rollout bound means zero in any of its spellings.
func isZeroBound(v *intstr.IntOrString) bool {
	if v == nil {
		return false
	}
	if v.Type == intstr.Int {
		return v.IntValue() == 0
	}
	return v.StrVal == "0" || v.StrVal == "0%"
}
