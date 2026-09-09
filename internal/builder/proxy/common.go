/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

// Package proxy provides helper functions for constructing the Kubernetes
// child resources owned by a Proxy custom resource.
package proxy

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
	"github.com/OmniTrustILM/operator/internal/builder/common"
)

// Label keys and operator constants used for child resource metadata. The shared
// keys alias internal/builder/common so a rename lands everywhere at once; only
// the proxy-specific key, component value, and checksum annotation are local.
const (
	ManagedByLabel     = common.ManagedByLabel
	NameLabel          = common.NameLabel
	ComponentLabel     = common.ComponentLabel
	ProxyLabel         = "otilm.com/proxy"
	ManagerName        = common.ManagedByValue
	ComponentValue     = "proxy"
	ChecksumAnnotation = "otilm.com/config-checksum"
)

// Container ports and env-var names of the proxy's config contract. The HTTP port
// serves /health, /ready, and /metrics; the API port serves the connector-facing
// registration endpoint (the future Connector proxyRef target). Both are the proxy
// binary's defaults — configuration inside the token must not change them, since the
// Service targets these fixed ports.
const (
	HTTPPort int32 = 8080
	APIPort  int32 = 8081

	EnvConfigToken     = "PROXY_CONFIG_TOKEN"      //nolint:gosec // G101: env-var NAME from the proxy's config contract, not a credential value
	EnvTokenSigningKey = "PROXY_TOKEN_SIGNING_KEY" //nolint:gosec // G101: env-var NAME from the proxy's config contract, not a credential value

	defaultTokenKey      = "configToken"
	defaultSigningKeyKey = "tokenSigningKey"
)

// Labels returns the full set of labels for child resources.
func Labels(px *otilmv1alpha1.Proxy) map[string]string {
	return map[string]string{
		NameLabel:      px.Name,
		ManagedByLabel: ManagerName,
		ComponentLabel: ComponentValue,
		ProxyLabel:     px.Name,
	}
}

// SelectorLabels returns labels used for pod selectors.
// These must be immutable after creation, so they exclude managed-by.
func SelectorLabels(px *otilmv1alpha1.Proxy) map[string]string {
	return map[string]string{
		NameLabel:      px.Name,
		ComponentLabel: ComponentValue,
		ProxyLabel:     px.Name,
	}
}

// ChildResourceName returns the name to use for child resources.
func ChildResourceName(px *otilmv1alpha1.Proxy) string {
	return px.Name
}

// TokenKey returns the Secret key holding the config token, applying the API default
// so builders are correct even for objects not round-tripped through the apiserver.
func TokenKey(px *otilmv1alpha1.Proxy) string {
	if k := px.Spec.ConfigTokenSecretRef.TokenKey; k != "" {
		return k
	}
	return defaultTokenKey
}

// SigningKeyKey returns the Secret key holding the optional HMAC signing key.
func SigningKeyKey(px *otilmv1alpha1.Proxy) string {
	if k := px.Spec.ConfigTokenSecretRef.SigningKeyKey; k != "" {
		return k
	}
	return defaultSigningKeyKey
}
