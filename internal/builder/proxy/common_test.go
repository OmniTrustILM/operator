/*
Copyright (c) ILM.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
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
