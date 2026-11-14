package v1alpha1

// AllowedTransitions is the node phase state machine: for each phase, the
// phases the agent may move to on its next evaluation (staying put is always
// allowed). The empty phase is a new object.
//
// The same table is in docs/design.md and a test keeps the two identical, and
// the agent's tests check every transition they cause against it.
var AllowedTransitions = map[Phase][]Phase{
	// A new object. The agent starts it Unmanaged; the others occur when the
	// controller created the object already carrying a target.
	"": {PhaseUnmanaged, PhasePending, PhaseUpToDate, PhaseBlocked},

	// Observing only. A policy claims the node, and the agent evaluates the
	// target for the first time.
	PhaseUnmanaged: {PhasePending, PhaseUpToDate, PhaseBlocked},

	// Differs from the target; waiting to be approved.
	PhasePending: {PhaseUnmanaged, PhaseUpToDate, PhaseBlocked, PhaseStaged, PhaseRebooting},

	// Pre-flight refused the update; nothing was written.
	PhaseBlocked: {PhaseUnmanaged, PhasePending, PhaseUpToDate, PhaseStaged, PhaseRebooting},

	// The update files are on the boot partition, waiting for the node to be
	// drained and cleared to reboot.
	PhaseStaged: {PhaseUnmanaged, PhasePending, PhaseUpToDate, PhaseBlocked, PhaseRebooting},

	// The agent has asked the kernel to reboot. After the reboot it is either
	// UpToDate or Failed.
	PhaseRebooting: {PhaseUnmanaged, PhasePending, PhaseUpToDate, PhaseBlocked, PhaseStaged, PhaseFailed},

	// The node rebooted but does not match. Sticky until the target changes or
	// the node is released.
	PhaseFailed: {PhaseUnmanaged, PhasePending, PhaseUpToDate, PhaseBlocked},

	// Matches the target.
	PhaseUpToDate: {PhaseUnmanaged, PhasePending, PhaseBlocked},
}

// CanTransition reports whether the agent may move a node from one phase to
// another. Staying in the same phase is always allowed.
func CanTransition(from, to Phase) bool {
	if from == to {
		return true
	}
	for _, p := range AllowedTransitions[from] {
		if p == to {
			return true
		}
	}
	return false
}
