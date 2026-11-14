package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
)

func piLabel() map[string]string { return map[string]string{"hw": "pi"} }

func (e *env) policy(name string) rpiv1.BootloaderPolicy {
	var p rpiv1.BootloaderPolicy
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: name}, &p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) events() string {
	var out []string
	for {
		select {
		case ev := <-e.rec.Events:
			out = append(out, ev)
		default:
			return strings.Join(out, "\n")
		}
	}
}

// A node selected by two policies belongs to whichever claimed it first; the
// second must not take it over or push its own target at it.
func TestSecondPolicyDoesNotStealANode(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()))
	e.reconcile()
	first := e.nb("pi-a").Spec.Target.Hash()

	other := policy()
	other.Name = "other"
	other.Spec.Config.BootOrder = []rpiv1.BootSource{rpiv1.BootUSB, rpiv1.BootRestart}
	if err := e.c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	nb := e.nb("pi-a")
	if nb.Spec.PolicyName != "pis" || nb.Spec.Target.Hash() != first {
		t.Fatalf("node was taken over: policy=%s", nb.Spec.PolicyName)
	}
	if ev := e.events(); !strings.Contains(ev, "NodeConflict") || !strings.Contains(ev, "pis") {
		t.Errorf("conflict not reported: %q", ev)
	}
	if st := e.policy("other").Status; st.Selected != 0 {
		t.Errorf("the losing policy claims %d nodes", st.Selected)
	}
}

// When a node stops matching, its object is removed, and a node this
// controller cordoned is returned to service rather than left stranded.
func TestUnselectedNodeIsReleased(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()), readyNode("pi-b", piLabel()))
	e.reconcile()
	e.agent("pi-a", rpiv1.PhasePending)
	e.agent("pi-b", rpiv1.PhasePending)
	e.reconcile() // cordons and approves pi-a

	var n corev1.Node
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: "pi-a"}, &n); err != nil {
		t.Fatal(err)
	}
	n.Labels = nil
	if err := e.c.Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	var list rpiv1.NodeBootloaderList
	_ = e.c.List(context.Background(), &list)
	if len(list.Items) != 1 || list.Items[0].Name != "pi-b" {
		t.Fatalf("expected only pi-b to remain, got %d objects", len(list.Items))
	}
	if !strings.Contains(strings.Join(e.d.log, ","), "uncordon:pi-a") {
		t.Fatalf("pi-a left cordoned: %v", e.d.log)
	}
}

func TestInvalidSelectorIsAnError(t *testing.T) {
	p := policy()
	p.Spec.NodeSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "hw", Operator: "Bogus"}}}
	e := setup(t, p)
	if _, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pis"}}); err == nil {
		t.Fatal("an invalid nodeSelector was accepted")
	}
}

func TestPausedPolicyCreatesObjectsButApprovesNothing(t *testing.T) {
	p := policy()
	p.Spec.Paused = true
	e := setup(t, p, readyNode("pi-a", piLabel()))
	e.reconcile()
	e.agent("pi-a", rpiv1.PhasePending)
	e.reconcile()
	if nb := e.nb("pi-a"); nb.Spec.ApprovedHash != "" || len(e.d.log) != 0 {
		t.Fatalf("a paused policy acted: approved=%q log=%v", nb.Spec.ApprovedHash, e.d.log)
	}
}

func TestNotReadyNodeIsNotApproved(t *testing.T) {
	down := readyNode("pi-a", piLabel())
	down.Status.Conditions[0].Status = corev1.ConditionFalse
	e := setup(t, policy(), down)
	e.reconcile()
	e.agent("pi-a", rpiv1.PhasePending)
	e.reconcile()
	if len(e.d.log) != 0 || e.nb("pi-a").Spec.ApprovedHash != "" {
		t.Fatalf("approved a node that is not Ready: %v", e.d.log)
	}
	noCond := readyNode("pi-b", piLabel())
	noCond.Status.Conditions = nil
	if nodeReady(noCond) {
		t.Error("a node with no Ready condition counted as ready")
	}
}

// A drain that cannot complete (e.g. a PodDisruptionBudget) must stop the
// rollout at that step: no reboot is authorised, and it is retried.
func TestFailedDrainBlocksTheReboot(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()))
	e.reconcile()
	e.agent("pi-a", rpiv1.PhasePending)
	e.reconcile()
	e.agent("pi-a", rpiv1.PhaseStaged)

	e.d.failDrain = true
	for i := 0; i < 3; i++ {
		e.reconcile()
	}
	if nb := e.nb("pi-a"); nb.Spec.RebootHash != "" {
		t.Fatal("authorised a reboot for a node that was never drained")
	}
	if !strings.Contains(e.events(), "ActionFailed") {
		t.Error("the failed drain was not reported")
	}
	e.d.failDrain = false
	e.reconcile() // drains
	e.reconcile() // authorises
	if nb := e.nb("pi-a"); nb.Spec.RebootHash != nb.Spec.Target.Hash() {
		t.Fatal("did not recover once the drain succeeded")
	}
}

// A node that never comes back halts the rollout and stays cordoned.
func TestRebootTimeoutHaltsAndKeepsTheNodeCordoned(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()), readyNode("pi-b", piLabel()))
	e.reconcile()
	e.agent("pi-a", rpiv1.PhasePending)
	e.agent("pi-b", rpiv1.PhasePending)
	e.reconcile() // approve pi-a
	e.agent("pi-a", rpiv1.PhaseStaged)
	e.reconcile() // drain
	e.reconcile() // authorise reboot
	e.agent("pi-a", rpiv1.PhaseRebooting)

	e.now = e.now.Add(time.Hour) // far beyond the 600s timeout
	e.reconcile()

	halted := false
	for _, c := range e.policy("pis").Status.Conditions {
		if c.Type == rpiv1.ConditionRolloutHalted && c.Status == metav1.ConditionTrue && strings.Contains(c.Message, "pi-a") {
			halted = true
		}
	}
	if !halted {
		t.Fatalf("rollout not halted: %+v", e.policy("pis").Status.Conditions)
	}
	if e.nb("pi-b").Spec.ApprovedHash != "" {
		t.Error("moved on to pi-b while pi-a was missing")
	}
	if strings.Contains(strings.Join(e.d.log, ","), "uncordon:pi-a") {
		t.Error("returned a node to service that never came back")
	}
}

func TestStatusConditionsWhenComplete(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()))
	e.reconcile()
	e.agent("pi-a", rpiv1.PhaseUpToDate)
	e.reconcile()
	st := e.policy("pis").Status
	complete := false
	for _, c := range st.Conditions {
		if c.Type == rpiv1.ConditionComplete && c.Status == metav1.ConditionTrue {
			complete = true
		}
	}
	if !complete || st.UpToDate != 1 || st.Selected != 1 {
		t.Fatalf("status = %+v", st)
	}
}
