/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

const (
	testProxyName      = "dc-east"
	testProxyNamespace = "ilm-edge"
)

func newProxy() *otilmv1alpha1.Proxy {
	return &otilmv1alpha1.Proxy{
		ObjectMeta: metav1.ObjectMeta{Name: testProxyName, Namespace: testProxyNamespace},
		Spec: otilmv1alpha1.ProxySpec{
			ConfigTokenSecretRef: otilmv1alpha1.ConfigTokenRef{Name: "dc-east-config"},
		},
	}
}

func TestLabels(t *testing.T) {
	px := newProxy()
	l := Labels(px)
	assert.Equal(t, testProxyName, l[NameLabel])
	assert.Equal(t, "ilm-operator", l[ManagedByLabel])
	assert.Equal(t, "proxy", l[ComponentLabel])
	assert.Equal(t, testProxyName, l[ProxyLabel])
}

func TestSelectorLabelsExcludeManagedBy(t *testing.T) {
	l := SelectorLabels(newProxy())
	_, has := l[ManagedByLabel]
	assert.False(t, has)
	assert.Equal(t, testProxyName, l[NameLabel])
}

func TestChildResourceName(t *testing.T) {
	assert.Equal(t, testProxyName, ChildResourceName(newProxy()))
}

func TestTokenKeyDefaults(t *testing.T) {
	px := newProxy()
	assert.Equal(t, "configToken", TokenKey(px))
	assert.Equal(t, "tokenSigningKey", SigningKeyKey(px))

	px.Spec.ConfigTokenSecretRef.TokenKey = "tok"
	px.Spec.ConfigTokenSecretRef.SigningKeyKey = "sig"
	assert.Equal(t, "tok", TokenKey(px))
	assert.Equal(t, "sig", SigningKeyKey(px))
}
