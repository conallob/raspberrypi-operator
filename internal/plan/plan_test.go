package plan

import (
	"testing"
	"time"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
)

var now = time.Unix(1_700_000_000, 0)

func n(name string, phase rpiv1.Phase, mod ...func(*Node)) Node {
	x := Node{Name: name, Ready: true, Hash: "h", StatusHash: "h", Phase: phase}
	for _, m := range mod {
		m(&x)
	}
	return x
}

func approved(x *Node) { x.Approved = true }
func cordoned(x *Node) { x.Cordoned = true }

func kinds(r Result) []string {
	var out []string
	for _, a := range r.Actions {
		out = append(out, string(a.Kind)+":"+a.Node)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestApprovesOneAtATime(t *testing.T) {
	r := Plan([]Node{
		n("c", rpiv1.PhasePending), n("a", rpiv1.PhasePending), n("b", rpiv1.PhasePending),
	}, Params{MaxUnavailable: 1, RebootTimeout: time.Hour, Now: now})
	if got := kinds(r); !equal(got, []string{"Approve:a"}) {
		t.Fatalf("got %v", got)
	}
	if r.Pending != 3 {
		t.Errorf("Pending = %d", r.Pending)
	}
}

func TestSlotsAccountForInFlight(t *testing.T) {
	r := Plan([]Node{
		n("a", rpiv1.PhaseRebooting, approved, cordoned),
		n("b", rpiv1.PhasePending),
		n("c", rpiv1.PhasePending),
	}, Params{MaxUnavailable: 2, RebootTimeout: time.Hour, Now: now})
	if got := kinds(r); !equal(got, []string{"Approve:b"}) {
		t.Fatalf("got %v", got)
	}
}

func TestStagedFlow(t *testing.T) {
	p := Params{MaxUnavailable: 1, RebootTimeout: time.Hour, Now: now}
	if got := kinds(Plan([]Node{n("a", rpiv1.PhaseStaged, approved, cordoned)}, p)); !equal(got, []string{"Drain:a"}) {
		t.Errorf("staged, undrained: %v", got)
	}
	drained := func(x *Node) { x.Drained = true }
	if got := kinds(Plan([]Node{n("a", rpiv1.PhaseStaged, approved, cordoned, drained)}, p)); !equal(got, []string{"AuthorizeReboot:a"}) {
		t.Errorf("staged, drained: %v", got)
	}
	auth := func(x *Node) { x.RebootAuthorized = true; x.RebootAuthorizedAt = now }
	if got := kinds(Plan([]Node{n("a", rpiv1.PhaseStaged, approved, cordoned, drained, auth)}, p)); len(got) != 0 {
		t.Errorf("already authorised: %v", got)
	}
}

func TestUncordonWhenDone(t *testing.T) {
	r := Plan([]Node{n("a", rpiv1.PhaseUpToDate, approved, cordoned), n("b", rpiv1.PhasePending)},
		Params{MaxUnavailable: 1, RebootTimeout: time.Hour, Now: now})
	if got := kinds(r); !equal(got, []string{"Uncordon:a", "Approve:b"}) {
		t.Fatalf("got %v", got)
	}
	// Not Ready yet: hold the cordon.
	notReady := func(x *Node) { x.Ready = false }
	r = Plan([]Node{n("a", rpiv1.PhaseUpToDate, approved, cordoned, notReady)}, Params{MaxUnavailable: 1, Now: now})
	if len(r.Actions) != 0 {
		t.Fatalf("uncordoned a node that is not Ready: %v", kinds(r))
	}
}

func TestFailureHaltsRollout(t *testing.T) {
	r := Plan([]Node{
		n("a", rpiv1.PhaseFailed, func(x *Node) { x.Message = "boom" }),
		n("b", rpiv1.PhasePending),
	}, Params{MaxUnavailable: 1, Now: now})
	if r.Halted == "" || r.Failed != 1 {
		t.Fatalf("expected halt, got %+v", r)
	}
	for _, a := range r.Actions {
		if a.Kind == Approve {
			t.Fatal("approved a node while halted")
		}
	}
}

func TestRebootTimeoutHalts(t *testing.T) {
	late := func(x *Node) {
		x.RebootAuthorized = true
		x.RebootAuthorizedAt = now.Add(-2 * time.Hour)
	}
	r := Plan([]Node{n("a", rpiv1.PhaseRebooting, approved, cordoned, late), n("b", rpiv1.PhasePending)},
		Params{MaxUnavailable: 2, RebootTimeout: time.Hour, Now: now})
	if r.Halted == "" {
		t.Fatal("expected a reboot-timeout halt")
	}
	for _, a := range r.Actions {
		if a.Kind == Approve {
			t.Fatal("approved a node while halted")
		}
	}
}

func TestPausedStillFinishesInFlight(t *testing.T) {
	r := Plan([]Node{n("a", rpiv1.PhaseStaged, approved, cordoned), n("b", rpiv1.PhasePending)},
		Params{MaxUnavailable: 2, Paused: true, Now: now})
	if got := kinds(r); !equal(got, []string{"Drain:a"}) {
		t.Fatalf("got %v", got)
	}
}

func TestStaleAgentStatusIsNotTrusted(t *testing.T) {
	stale := func(x *Node) { x.StatusHash = "old" }
	r := Plan([]Node{n("a", rpiv1.PhasePending, stale)}, Params{MaxUnavailable: 1, Now: now})
	if len(r.Actions) != 0 {
		t.Fatalf("acted on stale status: %v", kinds(r))
	}
}

func TestBlockedAfterApprovalIsRevoked(t *testing.T) {
	r := Plan([]Node{n("a", rpiv1.PhaseBlocked, approved, cordoned)}, Params{MaxUnavailable: 1, Now: now})
	if got := kinds(r); !equal(got, []string{"Revoke:a"}) {
		t.Fatalf("got %v", got)
	}
}

func TestTargetChangedMidFlightReleasesNode(t *testing.T) {
	// Cordoned, but approval no longer matches the (new) target hash.
	r := Plan([]Node{n("a", rpiv1.PhasePending, cordoned)}, Params{MaxUnavailable: 1, Now: now})
	if got := kinds(r); len(got) == 0 || got[0] != "Revoke:a" {
		t.Fatalf("got %v", got)
	}
}

func TestBlockedAndNotReadyNodesAreSkipped(t *testing.T) {
	r := Plan([]Node{
		n("a", rpiv1.PhaseBlocked),
		n("b", rpiv1.PhasePending, func(x *Node) { x.Ready = false }),
		n("c", rpiv1.PhasePending),
	}, Params{MaxUnavailable: 1, Now: now})
	if got := kinds(r); !equal(got, []string{"Approve:c"}) || r.Blocked != 1 {
		t.Fatalf("got %v blocked=%d", got, r.Blocked)
	}
}
