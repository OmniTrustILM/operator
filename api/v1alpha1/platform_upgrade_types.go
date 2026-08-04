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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MigrationPhase is the stage of an in-flight messaging migration.
// +kubebuilder:validation:Enum=Fencing;Draining;CuttingOver;CleaningUp
type MigrationPhase string

// Messaging-migration phase constants, in the order the engine walks them: Fencing (scale
// the platform's producers/consumers to zero so no new traffic lands on the source vhost),
// Draining (wait for the source vhost's queues to empty), CuttingOver (repoint the
// platform's messaging connection at the target-version vhost), and CleaningUp (tear down
// the source-version topology).
const (
	MigrationPhaseFencing     MigrationPhase = "Fencing"
	MigrationPhaseDraining    MigrationPhase = "Draining"
	MigrationPhaseCuttingOver MigrationPhase = "CuttingOver"
	MigrationPhaseCleaningUp  MigrationPhase = "CleaningUp"
)

// FencedWorkload records one workload the fence scaled to zero, so Resume can restore
// exactly what was there. Kind distinguishes a Deployment from a StatefulSet gateway.
type FencedWorkload struct {
	// Name is the workload's object name.
	Name string `json:"name"`
	// Kind is the workload's Kind: "Deployment" or "StatefulSet".
	Kind string `json:"kind"`
	// Replicas is the replica count the workload carried immediately before the fence
	// scaled it to zero, restored verbatim on Resume.
	Replicas int32 `json:"replicas"`
}

// UpgradeStatus is the in-flight messaging-migration state. It exists ONLY while a
// migration is running: the engine writes it before every side effect so a crash at any
// point resumes deterministically, and clears it when the migration completes.
type UpgradeStatus struct {
	// FromVersion is the platform version bundle the migration is moving away from.
	FromVersion string `json:"fromVersion"`
	// ToVersion is the platform version bundle the migration is cutting over to.
	ToVersion string `json:"toVersion"`
	// Phase is the current stage of the migration.
	Phase MigrationPhase `json:"phase"`
	// Fenced lists the workloads the Fencing phase scaled to zero, so Resume can restore
	// each one's original replica count.
	// +optional
	Fenced []FencedWorkload `json:"fenced,omitempty"`
	// CleanDrainPolls counts consecutive polls that observed every drainable queue empty.
	// +optional
	CleanDrainPolls int32 `json:"cleanDrainPolls,omitempty"`
	// StartedAt is when the migration began (the start of the Fencing phase).
	StartedAt metav1.Time `json:"startedAt"`
	// PhaseStartedAt bounds the current phase's deadline independently of the whole run.
	PhaseStartedAt metav1.Time `json:"phaseStartedAt"`
}
