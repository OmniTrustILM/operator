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

package platform

import (
	"testing"

	"github.com/OmniTrustILM/operator/pkg/bom"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildMessagingConfigMap(t *testing.T) {
	w := bom.Wiring()
	p := basePlatform()
	cm := BuildMessagingConfigMap(p)

	assert.Equal(t, w.MessagingConfigMapName, cm.Name)
	assert.Equal(t, "ilm-system", cm.Namespace, "namespace propagates from the CR")
	assert.Equal(t, messagingConfigMapRole, cm.Labels[common.ComponentLabel],
		"component label makes the object selectable by role")

	require.Contains(t, cm.Data, w.MessagingHostKey)
	require.Contains(t, cm.Data, w.MessagingPortKey)
	assert.Equal(t, "mq.example.com", cm.Data[w.MessagingHostKey])
	assert.Equal(t, "5672", cm.Data[w.MessagingPortKey])

	// Broker credentials are never published into the ConfigMap.
	assert.NotContains(t, cm.Data, "username")
	assert.NotContains(t, cm.Data, "password")
}

func TestBuildFeAdministratorConfigMapDefaults(t *testing.T) {
	cm := BuildFeAdministratorConfigMap(basePlatform())

	assert.Equal(t, feConfigMapName, cm.Name)
	assert.Equal(t, "ilm-system", cm.Namespace)
	assert.Equal(t, feAdministratorName, cm.Labels[common.ComponentLabel],
		"component label makes the object selectable by role")

	require.Contains(t, cm.Data, feConfigFile)
	js := cm.Data[feConfigFile]
	// config.js carries the runtime window.__ENV__ with default URLs and proxies off.
	assert.Contains(t, js, "window.__ENV__")
	assert.Contains(t, js, `"API_URL": "/api"`)
	assert.Contains(t, js, `"LOGIN_URL": "/login"`)
	assert.Contains(t, js, `"LOGOUT_URL": "/logout"`)
	assert.Contains(t, js, `"ENABLE_PROXIES": false`)
}

func TestBuildFeAdministratorConfigMapCustomURLsAndProxy(t *testing.T) {
	p := basePlatform()
	p.Spec.FeAdministrator.URL.API = "/custom-api"
	p.Spec.FeAdministrator.URL.Login = "/custom-login"
	p.Spec.FeAdministrator.URL.Logout = "/custom-logout"
	p.Spec.Common.Proxy.Enabled = true

	cm := BuildFeAdministratorConfigMap(p)
	js := cm.Data[feConfigFile]
	assert.Contains(t, js, `"API_URL": "/custom-api"`)
	assert.Contains(t, js, `"LOGIN_URL": "/custom-login"`)
	assert.Contains(t, js, `"LOGOUT_URL": "/custom-logout"`)
	assert.Contains(t, js, `"ENABLE_PROXIES": true`, "ENABLE_PROXIES tracks spec.proxy.enabled")
}
