/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

import (
	"testing"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/stretchr/testify/assert"
)

// testRegistry is the shared registry host used across the repository-precedence
// test cases below.
const testRegistry = "reg.example.com"

func TestResolveImagePrefersComponentOverShared(t *testing.T) {
	shared := otilmv1alpha1.ImageSpec{Registry: "shared.io", Repository: "ilm", PullPolicy: "IfNotPresent"}
	comp := otilmv1alpha1.ImageSpec{Name: "core", Repository: "team", Tag: "9.9.9"}
	ref, policy := ResolveImage(bom.Lookup, "core", shared, comp)
	assert.Equal(t, "shared.io/team/core:9.9.9", ref)
	assert.Equal(t, "IfNotPresent", string(policy))
}

func TestResolveImageFallsBackToBOMTag(t *testing.T) {
	shared := otilmv1alpha1.ImageSpec{Registry: "hub.omnitrustregistry.com", Repository: "ilm"}
	ref, _ := ResolveImage(bom.Lookup, "core", shared, otilmv1alpha1.ImageSpec{Name: "core"})
	assert.Equal(t, "hub.omnitrustregistry.com/ilm/core:2.19.0", ref)
}

func TestResolveImageConnectorStyleRepositoryTag(t *testing.T) {
	// No registry/name (Connector-style): the non-empty segments join to repository:tag.
	// nil bundleLookup: the version-agnostic Connector has no BOM bundle to fall back to.
	ref, policy := ResolveImage(nil, "", otilmv1alpha1.ImageSpec{},
		otilmv1alpha1.ImageSpec{Repository: "harbor.example.com/ilm/connector", Tag: "1.2.3"})
	assert.Equal(t, "harbor.example.com/ilm/connector:1.2.3", ref)
	assert.Equal(t, "IfNotPresent", string(policy)) // default when unset
}

func TestResolveImageSharedFillsWhatComponentOmits(t *testing.T) {
	// comp sets repository; shared supplies the tag → per-field merge.
	shared := otilmv1alpha1.ImageSpec{Tag: "1.0"}
	ref, _ := ResolveImage(nil, "", shared, otilmv1alpha1.ImageSpec{Repository: "r"})
	assert.Equal(t, "r:1.0", ref)
}

func TestResolveImageBOMFillsName(t *testing.T) {
	// Known component, no name/tag on comp/shared → both filled from the bundle.
	ref, _ := ResolveImage(bom.Lookup, "core", otilmv1alpha1.ImageSpec{Repository: "ilm"}, otilmv1alpha1.ImageSpec{})
	assert.Equal(t, "ilm/core:2.19.0", ref)
}

// TestResolveImageNilLookupSkipsBundleFallback proves a nil bundleLookup (the
// version-agnostic Connector path) never fills a name/tag from any bundle, even for a
// component name that the default bundle WOULD resolve.
func TestResolveImageNilLookupSkipsBundleFallback(t *testing.T) {
	ref, _ := ResolveImage(nil, "core", otilmv1alpha1.ImageSpec{Repository: "ilm"}, otilmv1alpha1.ImageSpec{})
	assert.Equal(t, "ilm", ref) // no name/tag filled — only the repository segment remains
}

func TestResolveImagePullPolicySharedWhenComponentEmpty(t *testing.T) {
	shared := otilmv1alpha1.ImageSpec{PullPolicy: "Always"}
	_, policy := ResolveImage(nil, "", shared, otilmv1alpha1.ImageSpec{Repository: "r", Tag: "1"})
	assert.Equal(t, "Always", string(policy))
}

func TestResolveImageDegenerateInputs(t *testing.T) {
	tests := []struct {
		name      string
		component string
		shared    otilmv1alpha1.ImageSpec
		comp      otilmv1alpha1.ImageSpec
		wantRef   string
	}{
		{"all empty", "", otilmv1alpha1.ImageSpec{}, otilmv1alpha1.ImageSpec{}, ""},
		{"only registry", "", otilmv1alpha1.ImageSpec{}, otilmv1alpha1.ImageSpec{Registry: "reg.io"}, "reg.io"},
		{"repository no tag", "", otilmv1alpha1.ImageSpec{}, otilmv1alpha1.ImageSpec{Repository: "repo"}, "repo"},
		{"tag only, no segments", "", otilmv1alpha1.ImageSpec{}, otilmv1alpha1.ImageSpec{Tag: "1.2.3"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, _ := ResolveImage(nil, tc.component, tc.shared, tc.comp)
			assert.Equal(t, tc.wantRef, ref)
		})
	}
}

func TestComponentLabelOverrides(t *testing.T) {
	legacySelector := map[string]string{"app.kubernetes.io/name": "x", "otilm.com/connector": "x"}
	legacyLabels := map[string]string{"app.kubernetes.io/name": "x", "otilm.com/connector": "x", "app.kubernetes.io/managed-by": "ilm-operator"}
	c := Component{
		Name:                   "x",
		LabelsOverride:         legacyLabels,
		SelectorLabelsOverride: legacySelector,
	}
	assert.Equal(t, legacySelector, c.SelectorLabels())
	assert.Equal(t, legacyLabels, c.Labels())

	// Without overrides the standard scheme applies unchanged.
	std := Component{Name: "core", Instance: "ilm"}
	assert.Equal(t, map[string]string{NameLabel: "core", InstanceLabel: "ilm"}, std.SelectorLabels())
}

func TestBuildDeploymentTerminationGracePeriod(t *testing.T) {
	grace := int64(90)
	c := Component{Name: "x", TerminationGracePeriodSeconds: &grace}
	dep := BuildDeployment(c)
	if assert.NotNil(t, dep.Spec.Template.Spec.TerminationGracePeriodSeconds) {
		assert.Equal(t, grace, *dep.Spec.Template.Spec.TerminationGracePeriodSeconds)
	}

	assert.Nil(t, BuildDeployment(Component{Name: "x"}).Spec.Template.Spec.TerminationGracePeriodSeconds)
}

// TestResolveImageRepositoryPrecedence pins the repository chain introduced for
// ilm-private components: component CR > bundle Repository (when shared is empty or
// the stock default) > shared CR > bom default — and proves the Connector path (nil
// lookup) and the lookup-miss path keep their no-default behavior.
func TestResolveImageRepositoryPrecedence(t *testing.T) {
	lookup := func(name string) (bom.Image, bool) {
		switch name {
		case "monitor":
			return bom.Image{Name: "time-quality-monitor", Tag: "1.0.0", Repository: "ilm-private"}, true
		case "core":
			return bom.Image{Name: "core", Tag: "2.18.0"}, true
		}
		return bom.Image{}, false
	}
	reg := otilmv1alpha1.ImageSpec{Registry: testRegistry}

	tests := []struct {
		name      string
		component string
		shared    otilmv1alpha1.ImageSpec
		comp      otilmv1alpha1.ImageSpec
		lookup    func(string) (bom.Image, bool)
		wantRef   string
	}{
		{"bundle repository wins when shared unset", "monitor", reg, otilmv1alpha1.ImageSpec{}, lookup,
			"reg.example.com/ilm-private/time-quality-monitor:1.0.0"},
		{"bundle repository wins over persisted/typed stock default", "monitor",
			otilmv1alpha1.ImageSpec{Registry: testRegistry, Repository: "ilm"}, otilmv1alpha1.ImageSpec{}, lookup,
			"reg.example.com/ilm-private/time-quality-monitor:1.0.0"},
		{"explicit non-default shared repository beats bundle", "monitor",
			otilmv1alpha1.ImageSpec{Registry: testRegistry, Repository: "mirror"}, otilmv1alpha1.ImageSpec{}, lookup,
			"reg.example.com/mirror/time-quality-monitor:1.0.0"},
		{"per-component repository beats everything", "monitor", reg, otilmv1alpha1.ImageSpec{Repository: "override"}, lookup,
			"reg.example.com/override/time-quality-monitor:1.0.0"},
		{"bom default fills bundle component without Repository", "core", reg, otilmv1alpha1.ImageSpec{}, lookup,
			"reg.example.com/ilm/core:2.18.0"},
		{"lookup miss keeps bare-name behavior", "absent", otilmv1alpha1.ImageSpec{Name: "x", Tag: "1"}, otilmv1alpha1.ImageSpec{}, lookup,
			"x:1"},
		{"nil lookup (Connector) keeps no repository default", "anything",
			otilmv1alpha1.ImageSpec{Name: "conn", Tag: "1"}, otilmv1alpha1.ImageSpec{}, nil,
			"conn:1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, _ := ResolveImage(tt.lookup, tt.component, tt.shared, tt.comp)
			assert.Equal(t, tt.wantRef, ref)
		})
	}
}

// TestMergePullSecrets pins the pod-level union: order-preserving, first occurrence
// wins, empties dropped — shared secrets first, per-component appended.
func TestMergePullSecrets(t *testing.T) {
	assert.Equal(t, []string{"a", "b", "c"}, MergePullSecrets([]string{"a", "b"}, []string{"b", "c", ""}))
	assert.Nil(t, MergePullSecrets(nil, nil))
	assert.Equal(t, []string{"x"}, MergePullSecrets(nil, []string{"x"}))
}
