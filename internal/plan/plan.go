// Package plan decides what the controller should do next for a set of nodes.
// It is pure: it takes a snapshot and returns actions, so every rollout rule
// is unit-testable without a cluster.
package plan

import (
	"fmt"
	"sort"
	"time"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
)

// Node is a snapshot of one node's rollout state.
type Node struct {
	Name  string
	Ready bool

	// Hash is the hash of the target in the node's spec.
	Hash string
	// StatusHash is the target hash the agent last evaluated. If it differs
	// from Hash the agent has not caught up and Phase must not be trusted.
	StatusHash string
	Phase      rpiv1.Phase
	Message    string

	Approved           bool // spec.approvedHash == Hash
	RebootAuthorized   bool // spec.rebootHash == Hash
	RebootAuthorizedAt time.Time

	// Cordoned: the controller cordoned this node for the rollout.
	Cordoned bool
	// Drained: the controller finished draining it for Hash.
	Drained bool
}

func (n Node) current() bool { return n.StatusHash == n.Hash }

// Kind is a type of action.
type Kind string

// Action kinds.
const (
	// Approve cordons the node, then lets the agent stage the update.
	Approve Kind = "Approve"
	// Drain evicts the node's pods once its update is staged.
	Drain Kind = "Drain"
	// AuthorizeReboot allows the agent to reboot a staged, drained node.
	AuthorizeReboot Kind = "AuthorizeReboot"
	// Uncordon returns a finished node to service.
	Uncordon Kind = "Uncordon"
	// Revoke withdraws an approval whose pre-flight no longer holds, and
	// returns the node to service.
	Revoke Kind = "Revoke"
)

// Action is one step for the controller to take.
type Action struct {
	Kind Kind
	Node string
}

// Params are the policy-level settings the planner needs.
type Params struct {
	MaxUnavailable int
	RebootTimeout  time.Duration
	Paused         bool
	Now            time.Time
}

// Result is the planner's output.
type Result struct {
	Actions []Action
	// Halted is non-empty when the rollout must stop for a human; it says why.
	Halted string

	UpToDate, Pending, InProgress, Blocked, Failed int
}

// Plan computes the next actions. Nodes are processed in name order so
// outcomes are deterministic.
func Plan(nodes []Node, p Params) Result {
	if p.MaxUnavailable < 1 {
		p.MaxUnavailable = 1
	}
	sorted := append([]Node(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var r Result
	inFlight := 0
	for _, n := range sorted {
		switch {
		case !n.current():
			// The agent hasn't evaluated this target yet: wait.
			r.Pending++
			if n.Cordoned {
				inFlight++
			}
			continue
		case n.Phase == rpiv1.PhaseFailed:
			r.Failed++
			if r.Halted == "" {
				r.Halted = fmt.Sprintf("node %s failed: %s", n.Name, n.Message)
			}
		case n.Phase == rpiv1.PhaseUpToDate:
			r.UpToDate++
		case n.Phase == rpiv1.PhaseBlocked && !n.Approved:
			r.Blocked++
		case n.Phase == rpiv1.PhasePending && !n.Approved:
			r.Pending++
		default:
			r.InProgress++
		}

		if n.RebootAuthorized && n.Phase != rpiv1.PhaseUpToDate && n.Phase != rpiv1.PhaseFailed &&
			!n.RebootAuthorizedAt.IsZero() && p.Now.Sub(n.RebootAuthorizedAt) > p.RebootTimeout && r.Halted == "" {
			r.Halted = fmt.Sprintf("node %s did not come back with the new bootloader within %s", n.Name, p.RebootTimeout)
		}

		// Slots: a node is unavailable while approved-and-unfinished or while
		// we hold it cordoned.
		if (n.Approved && n.Phase != rpiv1.PhaseUpToDate) || (n.Cordoned && n.Phase != rpiv1.PhaseUpToDate) {
			inFlight++
		}
	}

	for _, n := range sorted {
		if !n.current() {
			continue
		}
		switch {
		case n.Phase == rpiv1.PhaseUpToDate && n.Cordoned:
			if n.Ready {
				r.Actions = append(r.Actions, Action{Uncordon, n.Name})
			}
		case n.Approved && n.Phase == rpiv1.PhaseBlocked:
			r.Actions = append(r.Actions, Action{Revoke, n.Name})
		case !n.Approved && n.Cordoned && n.Phase != rpiv1.PhaseRebooting:
			// Target changed under an in-flight node: release it; it will be
			// re-approved against the new target if it still needs to be.
			r.Actions = append(r.Actions, Action{Revoke, n.Name})
		case n.Approved && n.Phase == rpiv1.PhaseStaged && !n.Drained:
			r.Actions = append(r.Actions, Action{Drain, n.Name})
		case n.Approved && n.Phase == rpiv1.PhaseStaged && n.Drained && !n.RebootAuthorized:
			r.Actions = append(r.Actions, Action{AuthorizeReboot, n.Name})
		}
	}

	if r.Halted != "" || p.Paused {
		return r
	}
	slots := p.MaxUnavailable - inFlight
	for _, n := range sorted {
		if slots <= 0 {
			break
		}
		if n.current() && n.Phase == rpiv1.PhasePending && !n.Approved && n.Ready {
			r.Actions = append(r.Actions, Action{Approve, n.Name})
			slots--
		}
	}
	return r
}
