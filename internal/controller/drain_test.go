package controller

import (
	"context"
	"io"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKubectlDrainerCordonUncordon(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "pi-1"}})
	d := KubectlDrainer{Client: cs, Out: io.Discard}
	ctx := context.Background()
	unschedulable := func() bool {
		n, err := cs.CoreV1().Nodes().Get(ctx, "pi-1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return n.Spec.Unschedulable
	}
	if err := d.Cordon(ctx, "pi-1"); err != nil || !unschedulable() {
		t.Fatalf("cordon: err=%v unschedulable=%v", err, unschedulable())
	}
	if err := d.Uncordon(ctx, "pi-1"); err != nil || unschedulable() {
		t.Fatalf("uncordon: err=%v unschedulable=%v", err, unschedulable())
	}
	// Cordoning twice is harmless.
	if err := d.Cordon(ctx, "pi-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Cordon(ctx, "pi-1"); err != nil || !unschedulable() {
		t.Fatalf("second cordon: %v", err)
	}
}

func TestKubectlDrainerEmptyNodeAndMissingNode(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "pi-1"}})
	d := KubectlDrainer{Client: cs, Out: io.Discard}
	ctx := context.Background()
	if err := d.Drain(ctx, "pi-1", time.Second); err != nil {
		t.Fatalf("draining a node with no pods: %v", err)
	}
	for name, err := range map[string]error{
		"cordon":   d.Cordon(ctx, "ghost"),
		"uncordon": d.Uncordon(ctx, "ghost"),
	} {
		if err == nil {
			t.Errorf("%s of a missing node succeeded", name)
		}
	}
	// Draining only lists the (absent) node's pods, so it succeeds vacuously.
	// That is safe because the controller always cordons first, and cordoning a
	// missing node fails (above).
	if err := d.Drain(ctx, "ghost", time.Second); err != nil {
		t.Errorf("drain of a missing node: %v", err)
	}
}
