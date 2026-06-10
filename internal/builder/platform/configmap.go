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
	"fmt"
	"strconv"

	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// messagingConfigMapRole is the component-label value carried by the messaging
// ConfigMap so a selector can match it by role.
const messagingConfigMapRole = "messaging"

// BuildMessagingConfigMap renders the broker-coordinates ConfigMap (non-secret).
// Core sources BROKER_HOST/BROKER_PORT from this via configMapKeyRef. Only the host
// and AMQP port are published here; the broker credentials remain in a Secret (never
// copied in). The host/port are resolved
// mode-agnostically (ResolveMessagingConnection): the caller's coordinates for an
// external broker, or the RabbitMQ Cluster Operator-generated Service + port 5672 for a
// managed one — so the ConfigMap is identical in shape for both modes.
func BuildMessagingConfigMap(p *otilmv1alpha1.Platform) *corev1.ConfigMap {
	w := wiringFor(p)
	conn := ResolveMessagingConnection(p)
	labels := map[string]string{
		common.NameLabel:      messagingConfigMapRole,
		common.InstanceLabel:  p.Name,
		common.ComponentLabel: messagingConfigMapRole,
		common.PartOfLabel:    common.PartOfValue,
		common.ManagedByLabel: common.ManagedByValue,
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      w.MessagingConfigMapName,
			Namespace: p.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			w.MessagingHostKey: conn.Host,
			w.MessagingPortKey: strconv.Itoa(int(conn.Port)),
		},
	}
}

// BuildFeAdministratorConfigMap renders the fe-administrator config.js ConfigMap.
// config.js assigns the runtime window.__ENV__ object the front-end reads at load
// time: the API/login/logout URLs (from spec.feAdministrator.url) and ENABLE_PROXIES
// (from spec.proxy.enabled). All values are non-sensitive routing/feature config.
func BuildFeAdministratorConfigMap(p *otilmv1alpha1.Platform) *corev1.ConfigMap {
	c := common.Component{Name: feAdministratorName, Instance: p.Name, Namespace: p.Namespace}

	api := p.Spec.FeAdministrator.URL.API
	if api == "" {
		api = defaultFeURLAPI
	}
	login := p.Spec.FeAdministrator.URL.Login
	if login == "" {
		login = defaultFeURLLogin
	}
	logout := p.Spec.FeAdministrator.URL.Logout
	if logout == "" {
		logout = defaultFeURLLogout
	}

	// config.js layout (window.__ENV__ object literal). Values are JSON-quoted;
	// ENABLE_PROXIES is the bare boolean from spec.proxy.enabled.
	configJS := fmt.Sprintf(`window.__ENV__ =
{
    "API_URL": %q,
    "LOGIN_URL": %q,
    "LOGOUT_URL": %q,
    "ENABLE_PROXIES": %t
}`, api, login, logout, p.Spec.Common.Proxy.Enabled)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      feConfigMapName,
			Namespace: p.Namespace,
			Labels:    c.Labels(),
		},
		Data: map[string]string{feConfigFile: configJS},
	}
}
