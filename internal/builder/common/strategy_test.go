/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

func intOrStr(v intstr.IntOrString) *intstr.IntOrString { return &v }

func TestBuildDeploymentStrategy(t *testing.T) {
	tests := []struct {
		name                string
		spec                *otilmv1alpha1.DeploymentStrategySpec
		wantNil             bool
		wantType            appsv1.DeploymentStrategyType
		wantSurge           *intstr.IntOrString
		wantUnavailable     *intstr.IntOrString
		wantNoRollingUpdate bool
	}{
		{
			name:    "unset spec renders no strategy",
			spec:    nil,
			wantNil: true,
		},
		{
			name:                "Recreate carries no rolling-update bounds",
			spec:                &otilmv1alpha1.DeploymentStrategySpec{Type: "Recreate"},
			wantType:            appsv1.RecreateDeploymentStrategyType,
			wantNoRollingUpdate: true,
		},
		{
			name:            "RollingUpdate without bounds materializes the apps/v1 defaults",
			spec:            &otilmv1alpha1.DeploymentStrategySpec{Type: "RollingUpdate"},
			wantType:        appsv1.RollingUpdateDeploymentStrategyType,
			wantSurge:       intOrStr(intstr.FromString("25%")),
			wantUnavailable: intOrStr(intstr.FromString("25%")),
		},
		{
			name: "explicit bounds are rendered verbatim",
			spec: &otilmv1alpha1.DeploymentStrategySpec{
				Type: "RollingUpdate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{
					MaxSurge:       intOrStr(intstr.FromInt32(0)),
					MaxUnavailable: intOrStr(intstr.FromInt32(2)),
				},
			},
			wantType:        appsv1.RollingUpdateDeploymentStrategyType,
			wantSurge:       intOrStr(intstr.FromInt32(0)),
			wantUnavailable: intOrStr(intstr.FromInt32(2)),
		},
		{
			name: "zero surge alone gains maxUnavailable 1",
			spec: &otilmv1alpha1.DeploymentStrategySpec{
				Type:          "RollingUpdate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{MaxSurge: intOrStr(intstr.FromInt32(0))},
			},
			wantType:        appsv1.RollingUpdateDeploymentStrategyType,
			wantSurge:       intOrStr(intstr.FromInt32(0)),
			wantUnavailable: intOrStr(intstr.FromInt32(1)),
		},
		{
			name: "zero surge as a percentage gains maxUnavailable 1",
			spec: &otilmv1alpha1.DeploymentStrategySpec{
				Type:          "RollingUpdate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{MaxSurge: intOrStr(intstr.FromString("0%"))},
			},
			wantType:        appsv1.RollingUpdateDeploymentStrategyType,
			wantSurge:       intOrStr(intstr.FromString("0%")),
			wantUnavailable: intOrStr(intstr.FromInt32(1)),
		},
		{
			name: "a leading-zero percentage counts as zero surge",
			spec: &otilmv1alpha1.DeploymentStrategySpec{
				Type:          "RollingUpdate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{MaxSurge: intOrStr(intstr.FromString("00%"))},
			},
			wantType:        appsv1.RollingUpdateDeploymentStrategyType,
			wantSurge:       intOrStr(intstr.FromString("00%")),
			wantUnavailable: intOrStr(intstr.FromInt32(1)),
		},
		{
			name: "a zero percentage of any width counts as zero surge",
			spec: &otilmv1alpha1.DeploymentStrategySpec{
				Type:          "RollingUpdate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{MaxSurge: intOrStr(intstr.FromString("000%"))},
			},
			wantType:        appsv1.RollingUpdateDeploymentStrategyType,
			wantSurge:       intOrStr(intstr.FromString("000%")),
			wantUnavailable: intOrStr(intstr.FromInt32(1)),
		},
		{
			name: "a leading zero on a non-zero percentage is not zero surge",
			spec: &otilmv1alpha1.DeploymentStrategySpec{
				Type:          "RollingUpdate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{MaxSurge: intOrStr(intstr.FromString("025%"))},
			},
			wantType:        appsv1.RollingUpdateDeploymentStrategyType,
			wantSurge:       intOrStr(intstr.FromString("025%")),
			wantUnavailable: intOrStr(intstr.FromString("25%")),
		},
		{
			name: "a non-zero surge gains the default maxUnavailable",
			spec: &otilmv1alpha1.DeploymentStrategySpec{
				Type:          "RollingUpdate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{MaxSurge: intOrStr(intstr.FromString("25%"))},
			},
			wantType:        appsv1.RollingUpdateDeploymentStrategyType,
			wantSurge:       intOrStr(intstr.FromString("25%")),
			wantUnavailable: intOrStr(intstr.FromString("25%")),
		},
		{
			name: "an explicit maxUnavailable gains the default maxSurge",
			spec: &otilmv1alpha1.DeploymentStrategySpec{
				Type: "RollingUpdate",
				RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{
					MaxUnavailable: intOrStr(intstr.FromInt32(2)),
				},
			},
			wantType:        appsv1.RollingUpdateDeploymentStrategyType,
			wantSurge:       intOrStr(intstr.FromString("25%")),
			wantUnavailable: intOrStr(intstr.FromInt32(2)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildDeploymentStrategy(tt.spec)

			if tt.wantNil {
				assert.Nil(t, got, "an unset spec must leave the apps/v1 default in place")
				return
			}

			require.NotNil(t, got)
			assert.Equal(t, tt.wantType, got.Type)

			if tt.wantNoRollingUpdate {
				assert.Nil(t, got.RollingUpdate)
				return
			}

			require.NotNil(t, got.RollingUpdate)
			assert.Equal(t, tt.wantSurge, got.RollingUpdate.MaxSurge)
			assert.Equal(t, tt.wantUnavailable, got.RollingUpdate.MaxUnavailable)
		})
	}
}

func TestBuildDeploymentStrategyCopiesBounds(t *testing.T) {
	surge := intstr.FromInt32(0)
	spec := &otilmv1alpha1.DeploymentStrategySpec{
		Type:          "RollingUpdate",
		RollingUpdate: &otilmv1alpha1.RollingUpdateSpec{MaxSurge: &surge},
	}

	rendered := BuildDeploymentStrategy(spec)
	require.NotNil(t, rendered.RollingUpdate)
	*rendered.RollingUpdate.MaxSurge = intstr.FromInt32(5)

	assert.Equal(t, intstr.FromInt32(0), surge, "rendering must not reach back into the CR")
}

func TestBuildDeploymentStrategyPrecedence(t *testing.T) {
	base := Component{Name: "core", Namespace: "ilm", Image: "img", Replicas: 1, Port: 8080}

	base.Strategy = &appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType}
	base.Recreate = true

	dep := BuildDeployment(base)
	assert.Equal(t, appsv1.RollingUpdateDeploymentStrategyType, dep.Spec.Strategy.Type,
		"a CR-supplied strategy wins over the DB-migration Recreate flag")
}
