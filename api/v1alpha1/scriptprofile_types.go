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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Script is a single custom script to run on the selected nodes.
type Script struct {
	// name identifies the script and names the file it is rendered into inside
	// the pod. It must be unique within a profile.
	// +required
	Name string `json:"name"`

	// content is the script body executed on each selected node.
	// +required
	Content string `json:"content"`

	// interpreter is the program used to run the script, e.g. "/bin/sh" or
	// "/bin/bash".
	// +kubebuilder:default="/bin/sh"
	// +optional
	Interpreter string `json:"interpreter,omitempty"`
}

// ScriptProfileSpec defines the desired state of ScriptProfile.
type ScriptProfileSpec struct {
	// nodeSelector selects the nodes this profile applies to.
	// +required
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// tolerations are applied to the script-runner DaemonSet pods so they can
	// schedule onto tainted nodes matched by nodeSelector.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// scripts is the list of custom scripts to run on the selected nodes, in
	// order.
	// +kubebuilder:validation:MinItems=1
	// +required
	Scripts []Script `json:"scripts"`
}

// ScriptProfileStatus defines the observed state of ScriptProfile.
type ScriptProfileStatus struct {
	// observedHash is the config hash the controller has reconciled against.
	// +optional
	ObservedHash string `json:"observedHash,omitempty"`

	// teardownHash is the config hash for which the runner DaemonSet has been
	// torn down after a fully successful rollout. When it differs from the
	// current config hash, the DaemonSet is (re)created to run the new scripts.
	// +optional
	TeardownHash string `json:"teardownHash,omitempty"`

	// desiredNodes is the number of nodes selected by nodeSelector.
	// +optional
	DesiredNodes int32 `json:"desiredNodes"`

	// appliedNodes is the number of nodes that ran the scripts at the desired hash.
	// +optional
	AppliedNodes int32 `json:"appliedNodes"`

	// failedNodes is the number of nodes that exhausted their retries.
	// +optional
	FailedNodes int32 `json:"failedNodes"`

	// nodeStatuses holds per-node state.
	// +listType=map
	// +listMapKey=nodeName
	// +optional
	NodeStatuses []NodeStatus `json:"nodeStatuses,omitempty"`

	// conditions represent the current state of the ScriptProfile resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredNodes`
// +kubebuilder:printcolumn:name="Applied",type=integer,JSONPath=`.status.appliedNodes`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedNodes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:metadata:annotations="api-approved.kubernetes.io=https://github.com/kubernetes/enhancements/pull/1111"

// ScriptProfile is the Schema for the scriptprofiles API
type ScriptProfile struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ScriptProfile
	// +required
	Spec ScriptProfileSpec `json:"spec"`

	// status defines the observed state of ScriptProfile
	// +optional
	Status ScriptProfileStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ScriptProfileList contains a list of ScriptProfile
type ScriptProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ScriptProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ScriptProfile{}, &ScriptProfileList{})
		return nil
	})
}
