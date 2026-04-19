// Package builder provides helper functions for constructing Kubernetes child resources.
package builder

import (
	otilmv1alpha1 "github.com/OmniTrustILM/operator/api/v1alpha1"
)

// Label keys and operator constants used for child resource metadata.
const (
	ManagedByLabel     = "app.kubernetes.io/managed-by"
	NameLabel          = "app.kubernetes.io/name"
	ComponentLabel     = "app.kubernetes.io/component"
	ConnectorLabel     = "otilm.com/connector"
	ManagerName        = "ilm-operator"
	ComponentValue     = "connector"
	ChecksumAnnotation = "otilm.com/config-checksum"
)

// Labels returns the full set of labels for child resources.
func Labels(conn *otilmv1alpha1.Connector) map[string]string {
	return map[string]string{
		NameLabel:      conn.Name,
		ManagedByLabel: ManagerName,
		ComponentLabel: ComponentValue,
		ConnectorLabel: conn.Name,
	}
}

// SelectorLabels returns labels used for pod selectors.
// These must be immutable after creation, so they exclude managed-by.
func SelectorLabels(conn *otilmv1alpha1.Connector) map[string]string {
	return map[string]string{
		NameLabel:      conn.Name,
		ComponentLabel: ComponentValue,
		ConnectorLabel: conn.Name,
	}
}

// ChildResourceName returns the name to use for child resources.
func ChildResourceName(conn *otilmv1alpha1.Connector) string {
	return conn.Name
}
