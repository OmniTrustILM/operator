/*
Copyright The ILM Authors.

SPDX-License-Identifier: Apache-2.0
*/

package common

// Standard Kubernetes recommended-label keys applied to every component's resources.
const (
	NameLabel      = "app.kubernetes.io/name"
	InstanceLabel  = "app.kubernetes.io/instance"
	ComponentLabel = "app.kubernetes.io/component"
	PartOfLabel    = "app.kubernetes.io/part-of"
	ManagedByLabel = "app.kubernetes.io/managed-by"

	// PartOfValue is the app.kubernetes.io/part-of value for all platform resources.
	PartOfValue = "ilm"
	// ManagedByValue is the app.kubernetes.io/managed-by value for operator-managed resources.
	ManagedByValue = "ilm-operator"

	partOfValue    = PartOfValue
	managedByValue = ManagedByValue
)
