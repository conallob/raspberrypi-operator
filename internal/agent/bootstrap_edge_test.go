package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
	"github.com/conallob/raspberrypi-operator/internal/node"
)

func (h *harness) intercept(f interceptor.Funcs) {
	h.r.Client = interceptor.NewClient(h.c.(client.WithWatch), f)
}

// An unmanaged node whose state cannot be read is still just Unmanaged: it is
// reported, not Blocked (there is nothing to block).
func TestUnmanagedNodeThatCannotBeProbed(t *testing.T) {
	h := bareHarness(t, rpiv1.ChipBCM2711, true)
	_ = h.r.Bootstrap(context.Background())
	h.r.Probe = func() (node.Info, error) { return node.Info{}, errors.New("device tree unreadable") }
	nb := h.reconcile()
	if nb.Status.Phase != rpiv1.PhaseUnmanaged || !strings.Contains(nb.Status.Message, "device tree unreadable") {
		t.Fatalf("phase=%s message=%q", nb.Status.Phase, nb.Status.Message)
	}
}

func TestUnmanagedNodeWithAnUnreadableConfigSaysSo(t *testing.T) {
	h := bareHarness(t, rpiv1.ChipBCM2712, true)
	_ = h.r.Bootstrap(context.Background())
	h.info.ConfigErr = node.ErrConfigUnavailable
	nb := h.reconcile()
	if nb.Status.Phase != rpiv1.PhaseUnmanaged || !strings.Contains(nb.Status.Message, "cannot be read") {
		t.Fatalf("phase=%s message=%q", nb.Status.Phase, nb.Status.Message)
	}
}

// The state machine is a safety net, not a gate: if the agent ever moves a node
// in a way the table forbids, that is logged as a bug, but the node still does
// the right thing rather than wedging.
func TestAnIllegalTransitionIsNotFatal(t *testing.T) {
	tg := target(rpiv1.ChipBCM2712, bo("nvme", "sd", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2712, tg)
	// Force a state that production cannot produce: Unmanaged status on an
	// object that already carries an approved target.
	nb := h.get()
	nb.Status.Phase = rpiv1.PhaseUnmanaged
	if err := h.c.Status().Update(context.Background(), &nb); err != nil {
		t.Fatal(err)
	}
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash() })
	if rpiv1.CanTransition(rpiv1.PhaseUnmanaged, rpiv1.PhaseStaged) {
		t.Fatal("setup: this transition is supposed to be forbidden")
	}
	if _, err := h.r.Reconcile(context.Background(), reqFor(nodeName)); err != nil {
		t.Fatal(err)
	}
	if got := h.get().Status.Phase; got != rpiv1.PhaseStaged || !staged(h) {
		t.Fatalf("the agent wedged on a forbidden transition: phase=%s staged=%v", got, staged(h))
	}
}

func TestAPIErrorsOnGetAreReturnedNotTreatedAsMissing(t *testing.T) {
	h := newHarness(t, rpiv1.ChipBCM2711, target(rpiv1.ChipBCM2711, bo("sd", "restart")))
	h.intercept(interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("connection refused")
	}})
	if _, err := h.r.Reconcile(context.Background(), reqFor(nodeName)); err == nil {
		t.Fatal("an API error was treated as a missing object")
	}
}

func TestBootstrapReportsAFailedCreate(t *testing.T) {
	h := bareHarness(t, rpiv1.ChipBCM2711, true)
	h.intercept(interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		return errors.New("quota exceeded")
	}})
	if err := h.r.ensureObject(context.Background()); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("err = %v", err)
	}
	// Bootstrap keeps trying until its context ends, and says why it stopped.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := h.r.Bootstrap(ctx); err == nil {
		t.Fatal("Bootstrap reported success although it could never create the object")
	}
}

// If the Node can be read but cannot be made the owner (a scheme mismatch), the
// object is not created: an unowned object would outlive its node.
func TestBootstrapDoesNotCreateWhenOwnershipFails(t *testing.T) {
	h := bareHarness(t, rpiv1.ChipBCM2711, false)
	// The reader knows the Node; the client's scheme does not.
	full := runtime.NewScheme()
	_ = corev1.AddToScheme(full)
	_ = rpiv1.AddToScheme(full)
	h.r.Reader = fake.NewClientBuilder().WithScheme(full).
		WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, UID: "u"}}).Build()
	narrow := runtime.NewScheme()
	_ = rpiv1.AddToScheme(narrow)
	h.r.Client = fake.NewClientBuilder().WithScheme(narrow).Build()
	if err := h.r.ensureObject(context.Background()); err == nil {
		t.Fatal("created an object it could not own")
	}
	var list rpiv1.NodeBootloaderList
	if err := h.r.Client.List(context.Background(), &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("objects=%d err=%v", len(list.Items), err)
	}
}

// The wiring: the reconciler and its startup step register against a manager.
// The API server is never contacted (the manager is not started).
func TestSetupWithManager(t *testing.T) {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = rpiv1.AddToScheme(sch)
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme:                 sch,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: mgr.GetClient(), NodeName: nodeName, Interval: time.Second}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
}
