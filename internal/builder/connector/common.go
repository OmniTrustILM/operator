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

// Package connector provides helper functions for constructing the Kubernetes
// child resources owned by a Connector custom resource.
package connector

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
