package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RolloutSpec controls how a policy is applied across nodes.
type RolloutSpec struct {
	// MaxUnavailable is how many nodes may be cordoned, updating or rebooting
	// at once.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxUnavailable int32 `json:"maxUnavailable,omitempty"`

	// DrainTimeoutSeconds bounds how long draining one node may take.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	// +optional
	DrainTimeoutSeconds int32 `json:"drainTimeoutSeconds,omitempty"`

	// RebootTimeoutSeconds is how long a node may take to come back with the
	// new bootloader after the reboot is authorised. When it expires the
	// rollout halts and the node stays cordoned.
	// +kubebuilder:default=900
	// +kubebuilder:validation:Minimum=1
	// +optional
	RebootTimeoutSeconds int32 `json:"rebootTimeoutSeconds,omitempty"`
}

// BootloaderPolicySpec declares the bootloader state for a set of nodes.
type BootloaderPolicySpec struct {
	// NodeSelector selects the nodes this policy manages. An empty selector
	// matches every node, which is almost never what you want: select on a
	// label such as kubernetes.io/arch=arm64 plus your own Pi label.
	// +optional
	NodeSelector metav1.LabelSelector `json:"nodeSelector,omitempty"`

	TargetSpec `json:",inline"`

	// +optional
	Rollout RolloutSpec `json:"rollout,omitempty"`

	// Paused stops new nodes from being approved. Nodes already in flight
	// finish.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// BootloaderPolicyStatus summarises the rollout.
type BootloaderPolicyStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Selected is the number of nodes the policy matches.
	Selected int32 `json:"selected"`
	// UpToDate nodes already match the target.
	UpToDate int32 `json:"upToDate"`
	// Pending nodes need an update and are waiting for a slot.
	Pending int32 `json:"pending"`
	// InProgress nodes are cordoned, staged or rebooting.
	InProgress int32 `json:"inProgress"`
	// Blocked nodes failed a pre-flight check; see their NodeBootloader.
	Blocked int32 `json:"blocked"`
	// Failed nodes were updated but did not come back as expected.
	Failed int32 `json:"failed"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types and reasons set on a BootloaderPolicy.
const (
	// ConditionRolloutHalted is True when the controller stopped approving
	// nodes because of a failure that needs a human.
	ConditionRolloutHalted = "RolloutHalted"
	// ConditionComplete is True when every selected node is up to date.
	ConditionComplete = "Complete"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=blp
// +kubebuilder:printcolumn:name="Selected",type=integer,JSONPath=`.status.selected`
// +kubebuilder:printcolumn:name="UpToDate",type=integer,JSONPath=`.status.upToDate`
// +kubebuilder:printcolumn:name="InProgress",type=integer,JSONPath=`.status.inProgress`
// +kubebuilder:printcolumn:name="Blocked",type=integer,JSONPath=`.status.blocked`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failed`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BootloaderPolicy declares the EEPROM firmware and bootloader configuration
// for a set of Raspberry Pi nodes.
type BootloaderPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BootloaderPolicySpec   `json:"spec,omitempty"`
	Status BootloaderPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BootloaderPolicyList is a list of BootloaderPolicy.
type BootloaderPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BootloaderPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BootloaderPolicy{}, &BootloaderPolicyList{})
}
