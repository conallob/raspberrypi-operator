package controller

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
)

func names(reqs []reconcile.Request) string {
	var out []string
	for _, r := range reqs {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// An event for a node decides which policies reconcile. A node nobody manages
// (an agent just created it) wakes every policy, since any might select it; a
// managed node wakes just its own.
func TestWatchesWakeTheRightPolicies(t *testing.T) {
	a, b := policy(), policy()
	a.Name, b.Name = "a", "b"
	e := setup(t, a, readyNode("pi-1", piLabel()))
	if err := e.c.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	unmanaged := &rpiv1.NodeBootloader{ObjectMeta: metav1.ObjectMeta{Name: "pi-1"}}
	if got := names(e.r.policiesForNodeBootloader(ctx, unmanaged)); got != "a,b" {
		t.Errorf("unmanaged node wakes %q, want every policy", got)
	}
	claimed := &rpiv1.NodeBootloader{ObjectMeta: metav1.ObjectMeta{Name: "pi-1"}, Spec: rpiv1.NodeBootloaderSpec{PolicyName: "b"}}
	if got := names(e.r.policiesForNodeBootloader(ctx, claimed)); got != "b" {
		t.Errorf("claimed node wakes %q, want only its policy", got)
	}
	if got := e.r.policiesForNodeBootloader(ctx, &corev1.Pod{}); got != nil {
		t.Errorf("an unrelated object wakes %v", got)
	}
	if got := names(e.r.policiesForNode(ctx, readyNode("pi-1", nil))); got != "a,b" {
		t.Errorf("a node label change wakes %q, want every policy", got)
	}
}

func TestWatchesSurviveAListFailure(t *testing.T) {
	sch := runtime.NewScheme()
	_ = rpiv1.AddToScheme(sch)
	c := fake.NewClientBuilder().WithScheme(sch).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("api server unavailable")
		},
	}).Build()
	r := &PolicyReconciler{Client: c}
	if got := r.allPolicyRequests(context.Background()); got != nil {
		t.Errorf("a failed list produced requests: %v", got)
	}
}

// If the node cannot be uncordoned, the spec must not be cleared: otherwise the
// controller would forget it had cordoned the node, and it would stay cordoned
// forever. The next reconcile retries.
func TestFailedUncordonKeepsTheNodeClaimed(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()))
	e.reconcile()
	e.agent("pi-a", rpiv1.PhasePending)
	e.reconcile() // cordons and approves pi-a

	var n corev1.Node
	if err := e.c.Get(context.Background(), clientKey("pi-a"), &n); err != nil {
		t.Fatal(err)
	}
	n.Labels = nil // no longer selected
	if err := e.c.Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}

	e.d.failUncordon = true
	if _, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: clientKey("pis")}); err == nil {
		t.Fatal("a failed uncordon was swallowed")
	}
	nb := e.nb("pi-a")
	if nb.Spec.PolicyName != "pis" || nb.Spec.ApprovedHash == "" || nb.Annotations[AnnotationCordoned] != "true" {
		t.Fatalf("the spec was cleared although the node is still cordoned: %+v %v", nb.Spec, nb.Annotations)
	}

	e.d.failUncordon = false
	e.reconcile()
	if nb := e.nb("pi-a"); nb.Spec.PolicyName != "" || nb.Annotations[AnnotationCordoned] != "" {
		t.Fatalf("did not recover once uncordon worked: %+v %v", nb.Spec, nb.Annotations)
	}
}

// Creating the object can fail (the API server is down); that is retried, not
// swallowed.
func TestObjectCreateFailureIsReturned(t *testing.T) {
	e := setup(t, policy(), readyNode("pi-a", piLabel()))
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = rpiv1.AddToScheme(sch)
	e.r.Client = interceptorClient(e, interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			if _, ok := obj.(*rpiv1.NodeBootloader); ok {
				return errors.New("quota exceeded")
			}
			return nil
		},
	})
	if _, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: clientKey("pis")}); err == nil {
		t.Fatal("a failed create was swallowed")
	}
}

// The wiring itself: the controller and its watches register against a manager.
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
	r := &PolicyReconciler{Client: mgr.GetClient(), Scheme: sch}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
}
