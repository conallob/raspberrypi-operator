package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase is where a node is in the update.
// +kubebuilder:validation:Enum=UpToDate;Pending;Blocked;Staged;Rebooting;Failed
type Phase string

const (
	// PhaseUpToDate: the running bootloader and config match the target.
	PhaseUpToDate Phase = "UpToDate"
	// PhasePending: the node differs from the target and awaits approval.
	PhasePending Phase = "Pending"
	// PhaseBlocked: a pre-flight check refused the update. Nothing was written.
	PhaseBlocked Phase = "Blocked"
	// PhaseStaged: the update files are on the boot partition; the next reboot
	// applies them.
	PhaseStaged Phase = "Staged"
	// PhaseRebooting: the agent has asked the kernel to reboot.
	PhaseRebooting Phase = "Rebooting"
	// PhaseFailed: the node rebooted but does not match the target. Sticky until
	// the target changes. The agent will not retry on its own.
	PhaseFailed Phase = "Failed"
)

// BootDevice is the device the node's boot partition lives on.
type BootDevice string

// Known boot devices.
const (
	BootDeviceUnknown BootDevice = "Unknown"
	BootDeviceSD      BootDevice = "SD"
	BootDeviceUSB     BootDevice = "USB"
	BootDeviceNVMe    BootDevice = "NVMe"
	BootDeviceNetwork BootDevice = "Network"
)

// NodeBootloaderSpec is written by the controller.
type NodeBootloaderSpec struct {
	// PolicyName is the BootloaderPolicy that owns this object.
	PolicyName string `json:"policyName"`

	// Target is a copy of the owning policy's target.
	Target TargetSpec `json:"target"`

	// ApprovedHash is the target hash the controller has cleared the agent to
	// stage. The agent stages nothing unless it equals the hash of Target.
	// +optional
	ApprovedHash string `json:"approvedHash,omitempty"`

	// RebootHash is the target hash for which the node is drained and may
	// reboot. The agent reboots only if it equals the hash of Target and of
	// what it staged.
	// +optional
	RebootHash string `json:"rebootHash,omitempty"`

	// RebootAuthorizedAt is when RebootHash was set; the reboot timeout runs
	// from here.
	// +optional
	RebootAuthorizedAt *metav1.Time `json:"rebootAuthorizedAt,omitempty"`
}

// NodeBootloaderStatus is written by the node agent.
type NodeBootloaderStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// Message explains Blocked and Failed.
	// +optional
	Message string `json:"message,omitempty"`

	// Observed state of the node.
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	Chip Chip `json:"chip,omitempty"`
	// BuildTimestamp is the running bootloader's build time (Unix seconds).
	// +optional
	BuildTimestamp int64 `json:"buildTimestamp,omitempty"`
	// +optional
	BootDevice BootDevice `json:"bootDevice,omitempty"`
	// BootOrder is the running bootloader's BOOT_ORDER.
	// +optional
	BootOrder string `json:"bootOrder,omitempty"`
	// BootID is the kernel boot ID; it changes on every reboot.
	// +optional
	BootID string `json:"bootID,omitempty"`

	// TargetHash is the hash of the target this status was computed for.
	// +optional
	TargetHash string `json:"targetHash,omitempty"`
	// StagedHash is the target hash whose files are on the boot partition.
	// +optional
	StagedHash string `json:"stagedHash,omitempty"`
	// RebootingFromBootID is the boot ID the agent rebooted away from. If the
	// boot ID has changed and the node still differs, the update failed.
	// +optional
	RebootingFromBootID string `json:"rebootingFromBootID,omitempty"`
	// FailedHash is the target hash that failed; the agent will not retry it.
	// +optional
	FailedHash string `json:"failedHash,omitempty"`

	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nbl
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.policyName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Chip",type=string,JSONPath=`.status.chip`
// +kubebuilder:printcolumn:name="Boot",type=string,JSONPath=`.status.bootDevice`
// +kubebuilder:printcolumn:name="Order",type=string,JSONPath=`.status.bootOrder`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NodeBootloader is the per-node working object shared by the controller and
// the node agent. Its name is the node name. The controller owns spec and the
// agent owns status; users normally only read it.
type NodeBootloader struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeBootloaderSpec   `json:"spec,omitempty"`
	Status NodeBootloaderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeBootloaderList is a list of NodeBootloader.
type NodeBootloaderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeBootloader `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeBootloader{}, &NodeBootloaderList{})
}
