/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// StrategyType controls how the operator applies and maintains sysctl values.
// +kubebuilder:validation:Enum=Audit;Once;Enforce
type StrategyType string

const (
	// StrategyAudit only reports drift; it never writes sysctl values.
	StrategyAudit StrategyType = "Audit"
	// StrategyOnce applies the values a single time per node and then stops.
	StrategyOnce StrategyType = "Once"
	// StrategyEnforce continuously re-applies on drift detection.
	StrategyEnforce StrategyType = "Enforce"
)

// Sysctl is a single kernel parameter to manage.
type Sysctl struct {
	// name is the sysctl key, e.g. "net.core.rmem_max".
	// It may contain the "{iface}" placeholder, which is expanded per matching
	// network interface using interfaceSelector.
	// +required
	Name string `json:"name"`

	// value is the desired value for the sysctl key.
	// +required
	Value string `json:"value"`

	// persistent, when true, also writes the value to a drop-in file under
	// /etc/sysctl.d so it survives reboots.
	// +optional
	Persistent bool `json:"persistent,omitempty"`

	// interfaceSelector expands a "{iface}" placeholder in name across the
	// matching network interfaces on each node.
	// +optional
	InterfaceSelector *InterfaceSelector `json:"interfaceSelector,omitempty"`
}

// InterfaceSelector selects which network interfaces a per-interface sysctl
// (one containing the "{iface}" placeholder) is expanded against.
type InterfaceSelector struct {
	// prefixes matches interface names by prefix, e.g. ["eth", "ens", "azure"].
	// +optional
	Prefixes []string `json:"prefixes,omitempty"`

	// exclude removes specific interface names from the match set,
	// e.g. ["lo", "cilium_host", "cilium_net"].
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// RolloutSpec controls how changes are rolled out across the selected nodes.
type RolloutSpec struct {
	// batchSize is the maximum number of nodes updated concurrently.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	BatchSize int32 `json:"batchSize,omitempty"`

	// batchInterval is the minimum wait between launching successive batches.
	// +optional
	BatchInterval metav1.Duration `json:"batchInterval,omitempty"`

	// failureThreshold is the number of failed nodes tolerated before the
	// rollout is considered failing.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// pauseOnFailure halts the rollout (stops launching new batches) once
	// failureThreshold is reached.
	// +optional
	PauseOnFailure bool `json:"pauseOnFailure,omitempty"`
}

// ScheduleSpec holds cron expressions that drive periodic work.
type ScheduleSpec struct {
	// driftCheck is a cron expression controlling how often nodes are audited
	// for drift from the desired sysctl values.
	// +optional
	DriftCheck string `json:"driftCheck,omitempty"`

	// reconcile is a cron expression controlling a full periodic reconcile.
	// +optional
	Reconcile string `json:"reconcile,omitempty"`
}

// StrategySpec selects the management strategy.
type StrategySpec struct {
	// type is one of Audit, Once, or Enforce.
	// +kubebuilder:default=Enforce
	// +optional
	Type StrategyType `json:"type,omitempty"`
}

// SysctlProfileSpec defines the desired state of SysctlProfile.
type SysctlProfileSpec struct {
	// nodeSelector selects the nodes this profile applies to.
	// +required
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// rollout controls batched rollout behavior.
	// +optional
	Rollout RolloutSpec `json:"rollout,omitempty"`

	// schedule holds cron expressions for periodic drift checks and reconciles.
	// +optional
	Schedule ScheduleSpec `json:"schedule,omitempty"`

	// strategy selects how values are applied and maintained.
	// +optional
	Strategy StrategySpec `json:"strategy,omitempty"`

	// sysctls is the list of kernel parameters to manage.
	// +kubebuilder:validation:MinItems=1
	// +required
	Sysctls []Sysctl `json:"sysctls"`

	// restoreOnDelete, when true, removes the persistent drop-in file from the
	// nodes when the profile is deleted.
	// +optional
	RestoreOnDelete bool `json:"restoreOnDelete,omitempty"`
}

// NodePhase summarizes the state of a single node for a profile.
type NodePhase string

const (
	NodePhasePending NodePhase = "Pending"
	NodePhaseApplied NodePhase = "Applied"
	NodePhaseFailed  NodePhase = "Failed"
	NodePhaseDrifted NodePhase = "Drifted"
	NodePhaseAudited NodePhase = "Audited"
)

// NodeStatus is the per-node observed state.
type NodeStatus struct {
	// nodeName is the name of the node.
	// +required
	NodeName string `json:"nodeName"`

	// phase is the current node phase.
	// +optional
	Phase NodePhase `json:"phase,omitempty"`

	// appliedHash is the config hash last successfully applied to the node.
	// +optional
	AppliedHash string `json:"appliedHash,omitempty"`

	// failCount is the number of consecutive apply failures observed.
	// +optional
	FailCount int32 `json:"failCount,omitempty"`

	// message carries the last human-readable detail for the node.
	// +optional
	Message string `json:"message,omitempty"`

	// lastTransitionTime is when the phase last changed.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// SysctlProfileStatus defines the observed state of SysctlProfile.
type SysctlProfileStatus struct {
	// observedHash is the config hash the controller has reconciled against.
	// +optional
	ObservedHash string `json:"observedHash,omitempty"`

	// teardownHash is the config hash for which the applier DaemonSet has been
	// torn down after a fully successful rollout. When it differs from the
	// current config hash, the DaemonSet is (re)created to apply the new config.
	// +optional
	TeardownHash string `json:"teardownHash,omitempty"`

	// desiredNodes is the number of nodes selected by nodeSelector.
	// +optional
	DesiredNodes int32 `json:"desiredNodes"`

	// appliedNodes is the number of nodes at the desired config hash.
	// +optional
	AppliedNodes int32 `json:"appliedNodes"`

	// failedNodes is the number of nodes that exhausted their retries.
	// +optional
	FailedNodes int32 `json:"failedNodes"`

	// driftedNodes is the number of nodes found drifted during the last audit.
	// +optional
	DriftedNodes int32 `json:"driftedNodes"`

	// erroredPods is the cumulative count of applier pods that ended in error.
	// +optional
	ErroredPods int32 `json:"erroredPods"`

	// lastRolloutTime is when the controller last launched applier pods.
	// +optional
	LastRolloutTime *metav1.Time `json:"lastRolloutTime,omitempty"`

	// nodeStatuses holds per-node state.
	// +listType=map
	// +listMapKey=nodeName
	// +optional
	NodeStatuses []NodeStatus `json:"nodeStatuses,omitempty"`

	// conditions represent the current state of the SysctlProfile resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.spec.strategy.type`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredNodes`
// +kubebuilder:printcolumn:name="Applied",type=integer,JSONPath=`.status.appliedNodes`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedNodes`
// +kubebuilder:printcolumn:name="ErroredPods",type=integer,JSONPath=`.status.erroredPods`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:metadata:annotations="api-approved.kubernetes.io=https://github.com/kubernetes/enhancements/pull/1111"

// SysctlProfile is the Schema for the sysctlprofiles API
type SysctlProfile struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SysctlProfile
	// +required
	Spec SysctlProfileSpec `json:"spec"`

	// status defines the observed state of SysctlProfile
	// +optional
	Status SysctlProfileStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SysctlProfileList contains a list of SysctlProfile
type SysctlProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SysctlProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SysctlProfile{}, &SysctlProfileList{})
		return nil
	})
}
