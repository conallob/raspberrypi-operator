package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
)

type fakeDrainer struct {
	log       []string
	failDrain bool
}

func (f *fakeDrainer) Cordon(_ context.Context, n string) error {
	f.log = append(f.log, "cordon:"+n)
	return nil
}
func (f *fakeDrainer) Uncordon(_ context.Context, n string) error {
	f.log = append(f.log, "uncordon:"+n)
	return nil
}
func (f *fakeDrainer) Drain(_ context.Context, n string, _ time.Duration) error {
	f.log = append(f.log, "drain:"+n)
	if f.failDrain {
		return errors.New("cannot evict: PodDisruptionBudget")
	}
	return nil
}

func readyNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

type env struct {
	t   *testing.T
	c   client.Client
	r   *PolicyReconciler
	d   *fakeDrainer
	rec *record.FakeRecorder
	now time.Time
}

func setup(t *testing.T, pol *rpiv1.BootloaderPolicy, nodes ...*corev1.Node) *env {
	t.Helper()
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = rpiv1.AddToScheme(sch)
	b := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&rpiv1.BootloaderPolicy{}, &rpiv1.NodeBootloader{}).WithObjects(pol)
	for _, n := range nodes {
		b = b.WithObjects(n)
	}
	c := b.Build()
	e := &env{t: t, c: c, d: &fakeDrainer{}, rec: record.NewFakeRecorder(100), now: time.Unix(1_800_000_000, 0)}
	e.r = &PolicyReconciler{Client: c, Scheme: sch, Drainer: e.d, Recorder: e.rec, Now: func() time.Time { return e.now }}
	return e
}

func policy() *rpiv1.BootloaderPolicy {
	return &rpiv1.BootloaderPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pis", Generation: 1},
		Spec: rpiv1.BootloaderPolicySpec{
			NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"hw": "pi"}},
			TargetSpec: rpiv1.TargetSpec{
				Firmware: rpiv1.FirmwareSpec{Images: []rpiv1.ImageSource{
					{Chip: rpiv1.ChipBCM2711, URL: "https://example.invalid/a", SHA256: strings.Repeat("a", 64), BuildTimestamp: 1_750_000_000},
					{Chip: rpiv1.ChipBCM2712, URL: "https://example.invalid/b", SHA256: strings.Repeat("b", 64), BuildTimestamp: 1_750_000_100},
				}},
				Config: rpiv1.BootloaderConfig{BootOrder: []rpiv1.BootSource{rpiv1.BootNVMe, rpiv1.BootSD, rpiv1.BootUSB, rpiv1.BootRestart}},
			},
			Rollout: rpiv1.RolloutSpec{MaxUnavailable: 1, RebootTimeoutSeconds: 600},
		},
	}
}

func (e *env) reconcile() {
	e.t.Helper()
	if _, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pis"}}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) nb(name string) rpiv1.NodeBootloader {
	var nb rpiv1.NodeBootloader
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: name}, &nb); err != nil {
		e.t.Fatalf("get %s: %v", name, err)
	}
	return nb
}

// agent plays the node agent: it reports a phase for the current target.
func (e *env) agent(name string, phase rpiv1.Phase) {
	e.t.Helper()
	nb := e.nb(name)
	nb.Status.Phase = phase
	nb.Status.TargetHash = nb.Spec.Target.Hash()
	if err := e.c.Status().Update(context.Background(), &nb); err != nil {
		e.t.Fatal(err)
	}
}

func TestRollsOutOneNodeAtATime(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", map[string]string{"hw": "pi"}), readyNode("pi-b", map[string]string{"hw": "pi"}), readyNode("x86", nil))

	e.reconcile()
	if len(e.d.log) != 0 {
		t.Fatalf("acted before the agent reported: %v", e.d.log)
	}
	if e.nb("pi-a").Spec.PolicyName != "pis" || e.nb("pi-b").Spec.PolicyName != "pis" {
		t.Fatal("NodeBootloaders not created")
	}
	var all rpiv1.NodeBootloaderList
	_ = e.c.List(context.Background(), &all)
	if len(all.Items) != 2 {
		t.Fatalf("selector leaked: %d NodeBootloaders", len(all.Items))
	}

	// Both nodes report Pending; only pi-a is approved, and it is cordoned first.
	e.agent("pi-a", rpiv1.PhasePending)
	e.agent("pi-b", rpiv1.PhasePending)
	e.reconcile()
	if strings.Join(e.d.log, ",") != "cordon:pi-a" {
		t.Fatalf("log = %v", e.d.log)
	}
	a, b := e.nb("pi-a"), e.nb("pi-b")
	if a.Spec.ApprovedHash != a.Spec.Target.Hash() || b.Spec.ApprovedHash != "" {
		t.Fatalf("approvals: a=%q b=%q", a.Spec.ApprovedHash, b.Spec.ApprovedHash)
	}

	// Staged -> drained -> reboot authorised, in separate steps.
	e.agent("pi-a", rpiv1.PhaseStaged)
	e.reconcile()
	if got := strings.Join(e.d.log, ","); got != "cordon:pi-a,drain:pi-a" {
		t.Fatalf("log = %s", got)
	}
	if e.nb("pi-a").Spec.RebootHash != "" {
		t.Fatal("authorised reboot in the same step as the drain")
	}
	e.reconcile()
	a = e.nb("pi-a")
	if a.Spec.RebootHash != a.Spec.Target.Hash() || a.Spec.RebootAuthorizedAt == nil {
		t.Fatalf("reboot not authorised: %+v", a.Spec)
	}

	// While pi-a is in flight, pi-b waits.
	e.agent("pi-a", rpiv1.PhaseRebooting)
	e.reconcile()
	if e.nb("pi-b").Spec.ApprovedHash != "" {
		t.Fatal("approved pi-b while pi-a was still updating")
	}

	// pi-a returns: uncordoned, then pi-b starts.
	e.agent("pi-a", rpiv1.PhaseUpToDate)
	e.reconcile()
	if got := strings.Join(e.d.log, ","); !strings.HasSuffix(got, "uncordon:pi-a,cordon:pi-b") {
		t.Fatalf("log = %s", got)
	}
	var pol rpiv1.BootloaderPolicy
	_ = e.c.Get(context.Background(), types.NamespacedName{Name: "pis"}, &pol)
	if pol.Status.Selected != 2 || pol.Status.UpToDate != 1 {
		t.Fatalf("status = %+v", pol.Status)
	}
}

func TestFailureHaltsAndKeepsNodeCordoned(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", map[string]string{"hw": "pi"}), readyNode("pi-b", map[string]string{"hw": "pi"}))
	e.reconcile()
	e.agent("pi-a", rpiv1.PhasePending)
	e.agent("pi-b", rpiv1.PhasePending)
	e.reconcile() // approves pi-a
	e.agent("pi-a", rpiv1.PhaseFailed)
	e.reconcile()
	for _, l := range e.d.log {
		if strings.HasSuffix(l, "pi-b") || l == "uncordon:pi-a" {
			t.Fatalf("unexpected action after failure: %v", e.d.log)
		}
	}
	var pol rpiv1.BootloaderPolicy
	_ = e.c.Get(context.Background(), types.NamespacedName{Name: "pis"}, &pol)
	if pol.Status.Failed != 1 {
		t.Fatalf("status = %+v", pol.Status)
	}
	halted := false
	for _, c := range pol.Status.Conditions {
		if c.Type == rpiv1.ConditionRolloutHalted && c.Status == metav1.ConditionTrue {
			halted = true
		}
	}
	if !halted {
		t.Fatal("RolloutHalted condition not set")
	}
}

func TestPolicyEditPropagatesAndInvalidatesApproval(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", map[string]string{"hw": "pi"}))
	e.reconcile()
	e.agent("pi-a", rpiv1.PhasePending)
	e.reconcile() // approved + cordoned
	oldHash := e.nb("pi-a").Spec.Target.Hash()

	var pol rpiv1.BootloaderPolicy
	_ = e.c.Get(context.Background(), types.NamespacedName{Name: "pis"}, &pol)
	pol.Spec.Config.BootOrder = []rpiv1.BootSource{rpiv1.BootSD, rpiv1.BootNVMe, rpiv1.BootUSB, rpiv1.BootRestart}
	if err := e.c.Update(context.Background(), &pol); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	a := e.nb("pi-a")
	if a.Spec.Target.Hash() == oldHash || a.Spec.Target.Config.BootOrder[0] != rpiv1.BootSD {
		t.Fatal("target not propagated")
	}
	if a.Spec.ApprovedHash == a.Spec.Target.Hash() {
		t.Fatal("old approval must not cover the new target")
	}
}

func TestDeletionUncordonsOnlyWhatWeCordoned(t *testing.T) {
	pol := policy()
	e := setup(t, pol, readyNode("pi-a", map[string]string{"hw": "pi"}), readyNode("pi-b", map[string]string{"hw": "pi"}))
	e.reconcile()
	e.agent("pi-a", rpiv1.PhasePending)
	e.agent("pi-b", rpiv1.PhasePending)
	e.reconcile() // cordons pi-a only

	if err := e.c.Delete(context.Background(), pol); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := strings.Join(e.d.log, ","); !strings.HasSuffix(got, "uncordon:pi-a") || strings.Contains(got, "uncordon:pi-b") {
		t.Fatalf("log = %s", got)
	}
}
