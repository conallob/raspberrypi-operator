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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// When a node stops matching, it goes back to Unmanaged rather than vanishing,
// and a node this controller cordoned is returned to service.
func TestUnselectedNodeGoesBackToUnmanaged(t *testing.T) {
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
	if len(list.Items) != 2 {
		t.Fatalf("a released node's object was deleted: %d objects remain", len(list.Items))
	}
	a := e.nb("pi-a")
	if a.Spec.PolicyName != "" || a.Spec.Target != nil || a.Spec.ApprovedHash != "" || a.Spec.RebootHash != "" ||
		a.Spec.RebootAuthorizedAt != nil || a.Annotations[AnnotationCordoned] != "" {
		t.Fatalf("pi-a was not returned to an empty, unmanaged spec: %+v annotations=%v", a.Spec, a.Annotations)
	}
	if e.nb("pi-b").Spec.PolicyName != "pis" {
		t.Error("pi-b, still selected, was disturbed")
	}
	if !strings.Contains(strings.Join(e.d.log, ","), "uncordon:pi-a") {
		t.Fatalf("pi-a left cordoned: %v", e.d.log)
	}
}

// Every Pi node already has an empty NodeBootloader from its agent; a policy
// that selects the node takes it over rather than creating a second one.
func TestPolicyAdoptsAnAgentCreatedObject(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()))
	agentMade := &rpiv1.NodeBootloader{ObjectMeta: metav1.ObjectMeta{Name: "pi-a"}}
	if err := e.c.Create(context.Background(), agentMade); err != nil {
		t.Fatal(err)
	}
	e.reconcile()

	nb := e.nb("pi-a")
	if nb.Spec.PolicyName != "pis" || nb.Spec.Target == nil || nb.Spec.TargetHash() != policy().Spec.Hash() {
		t.Fatalf("not adopted: %+v", nb.Spec)
	}
	var list rpiv1.NodeBootloaderList
	_ = e.c.List(context.Background(), &list)
	if len(list.Items) != 1 {
		t.Fatalf("%d objects, want 1", len(list.Items))
	}
	// Nothing is approved until the agent has evaluated the new target.
	if nb.Spec.ApprovedHash != "" || len(e.d.log) != 0 {
		t.Errorf("acted before the agent reported: approved=%q log=%v", nb.Spec.ApprovedHash, e.d.log)
	}
}

// An unmanaged node that no policy selects is left exactly as it is.
func TestUnselectedUnmanagedNodesAreLeftAlone(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()), readyNode("pi-other", map[string]string{"hw": "other"}))
	other := &rpiv1.NodeBootloader{ObjectMeta: metav1.ObjectMeta{Name: "pi-other"}}
	if err := e.c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	got := e.nb("pi-other")
	if got.Spec.PolicyName != "" || got.Spec.Target != nil {
		t.Fatalf("a node outside the selector was claimed: %+v", got.Spec)
	}
}

// A node left claimed by a policy that no longer exists (its finalizer was
// removed by hand, say) can be adopted by another policy.
func TestClaimByAMissingPolicyIsAdopted(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()))
	stale := &rpiv1.NodeBootloader{
		ObjectMeta: metav1.ObjectMeta{Name: "pi-a"},
		Spec:       rpiv1.NodeBootloaderSpec{PolicyName: "long-gone", Target: &rpiv1.TargetSpec{}},
	}
	if err := e.c.Create(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if nb := e.nb("pi-a"); nb.Spec.PolicyName != "pis" {
		t.Fatalf("still claimed by %q", nb.Spec.PolicyName)
	}
}

// When the controller has to create the object itself (the agent has not
// started yet), the Node owns it, as it does for the agent's.
func TestControllerCreatedObjectIsOwnedByTheNode(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()))
	e.reconcile()
	nb := e.nb("pi-a")
	if len(nb.OwnerReferences) != 1 || nb.OwnerReferences[0].Kind != "Node" || nb.OwnerReferences[0].Name != "pi-a" {
		t.Fatalf("owner references = %+v", nb.OwnerReferences)
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

func clientKey(name string) types.NamespacedName { return types.NamespacedName{Name: name} }

// interceptorClient wraps the harness's fake client with failure injection.
func interceptorClient(e *env, f interceptor.Funcs) client.Client {
	return interceptor.NewClient(e.c.(client.WithWatch), f)
}
