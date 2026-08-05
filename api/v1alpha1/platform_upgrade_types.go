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
// the platform's message PRODUCERS — the gateway, the scheduler and the bundled provisioning
// service — to zero so no new traffic lands on the source vhost; Core, the consumer that
// empties the queues, deliberately keeps running), Draining (wait for the source vhost's
// queues to empty), CuttingOver (repoint the platform's messaging connection at the
// target-version vhost), and CleaningUp (tear down the source-version topology).
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
	// Absent records that the workload did not exist when the fence recorded its targets.
	// The entry is recorded ANYWAY: the fence's list is the complete set of producers the
	// migration intends to hold down, so a workload the ordinary render creates (or
	// re-creates) mid-migration is fenced rather than silently allowed to start publishing
	// outside the fence. It also tells the engine what "stopped" means for this entry —
	// still absent, rather than observed at zero replicas — and that there is no replica
	// count to write back when the fence is lifted.
	// +optional
	Absent bool `json:"absent,omitempty"`
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
	// NextDrainPollAt is the earliest time the drain may take its NEXT sample. The
	// consecutive-clean-poll count is only meaningful if the samples are SPACED: every
	// status write re-enqueues the Platform through the operator's own watch, so without an
	// explicit floor three "consecutive" polls could all land inside the same second and
	// prove nothing about a quiet virtual host. A poll arriving before this time is ignored.
	// +optional
	NextDrainPollAt *metav1.Time `json:"nextDrainPollAt,omitempty"`
	// InputsHash is a coordinate-free fingerprint (a hash — never the values themselves) of
	// the migration-relevant spec inputs as they stood when the migration began: the
	// messaging mode and wiring, the source and target virtual hosts, and the provisioning
	// mode. The engine re-computes it every reconcile and refuses to proceed when it has
	// changed, because those inputs decide which broker is addressed and which topology
	// objects are the source and which the target — editing them mid-flight could complete a
	// migration "successfully" while leaving the original topology orphaned.
	// +optional
	InputsHash string `json:"inputsHash,omitempty"`
	// ForceCarriedOver records that spec.messaging.managed.forceCutoverForVersion ALREADY
	// named this migration's target version when the attempt began — i.e. it is a value left
	// behind by an earlier attempt, which cannot authorize this one. It clears the moment the
	// field is cleared or changed, so re-setting it deliberately for this attempt counts.
	// +optional
	ForceCarriedOver bool `json:"forceCarriedOver,omitempty"`
	// ForceAuthorized records that this attempt has CONSUMED the destructive force-cutover
	// authorization. It is persisted before the first step the authorization permits, so the
	// cleanup still honours a force the cutover acted on even if the spec field is cleared in
	// between.
	// +optional
	ForceAuthorized bool `json:"forceAuthorized,omitempty"`
	// StartedAt is when the migration began (the start of the Fencing phase).
	StartedAt metav1.Time `json:"startedAt"`
	// PhaseStartedAt bounds the current phase's deadline independently of the whole run.
	PhaseStartedAt metav1.Time `json:"phaseStartedAt"`
}
