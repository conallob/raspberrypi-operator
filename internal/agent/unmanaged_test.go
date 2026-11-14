package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
	"github.com/conallob/raspberrypi-operator/internal/node"
)

// bareHarness is an agent whose node has no NodeBootloader yet, as on first
// start. withNode controls whether the Node object exists.
func bareHarness(t *testing.T, chip rpiv1.Chip, withNode bool) *harness {
	t.Helper()
	h := newHarness(t, chip, target(chip, bo("sd", "restart")))
	// Start over without the object newHarness created.
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	if err := rpiv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(sch).WithStatusSubresource(&rpiv1.NodeBootloader{})
	if withNode {
		b = b.WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, UID: "node-uid"}})
	}
	h.c = b.Build()
	h.r.Client = h.c
	return h
}

func (h *harness) exists() bool {
	var nb rpiv1.NodeBootloader
	err := h.c.Get(context.Background(), types.NamespacedName{Name: nodeName}, &nb)
	if err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatal(err)
	}
	return err == nil
}

// A node no policy selects is visible, observed, and left strictly alone.
func TestUnmanagedNodeIsObservedAndNeverTouched(t *testing.T) {
	for _, chip := range []rpiv1.Chip{rpiv1.ChipBCM2711, rpiv1.ChipBCM2712} {
		t.Run(string(chip), func(t *testing.T) {
			h := bareHarness(t, chip, true)
			if err := h.r.Bootstrap(context.Background()); err != nil {
				t.Fatal(err)
			}
			nb := h.reconcile()
			if nb.Status.Phase != rpiv1.PhaseUnmanaged {
				t.Fatalf("phase = %s (%s)", nb.Status.Phase, nb.Status.Message)
			}
			if nb.Status.Chip != chip || nb.Status.BuildTimestamp != h.info.BuildTimestamp ||
				nb.Status.BootDevice != rpiv1.BootDeviceSD || nb.Status.BootOrder != "sd,usb,restart" {
				t.Errorf("not observed: %+v", nb.Status)
			}
			if nb.Status.TargetHash != "" || nb.Spec.PolicyName != "" || nb.Spec.Target != nil {
				t.Errorf("an unmanaged node carries a target: %+v %+v", nb.Spec, nb.Status)
			}
			for i := 0; i < 3; i++ {
				h.reconcile()
			}
			if staged(h) || h.fetch.calls != 0 || h.reboot.n != 0 {
				t.Fatalf("an unmanaged node was acted on: staged=%v fetches=%d reboots=%d", staged(h), h.fetch.calls, h.reboot.n)
			}
		})
	}
}

// An update file a person staged by hand is not ours to remove.
func TestUnmanagedLeavesSomeoneElsesStagedUpdateAlone(t *testing.T) {
	h := bareHarness(t, rpiv1.ChipBCM2712, true)
	_ = h.r.Bootstrap(context.Background())
	for _, f := range []string{node.UpdateFile, node.SigFile} {
		if err := os.WriteFile(filepath.Join(h.bootfs, f), []byte("manual"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.reconcile()
	h.reconcile()
	if !staged(h) {
		t.Fatal("the agent deleted an update it did not stage")
	}
}

// A policy claims the node, then lets go of it: back to Unmanaged, with anything
// the agent staged removed and no reboot.
func TestReleasedNodeReturnsToUnmanaged(t *testing.T) {
	tg := target(rpiv1.ChipBCM2712, bo("nvme", "sd", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2712, tg)
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash() })
	if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhaseStaged || !staged(h) {
		t.Fatalf("setup: phase=%s", nb.Status.Phase)
	}
	// The policy is deleted: the controller clears the whole spec.
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec = rpiv1.NodeBootloaderSpec{} })
	nb := h.reconcile()
	if nb.Status.Phase != rpiv1.PhaseUnmanaged || nb.Status.StagedHash != "" || nb.Status.TargetHash != "" {
		t.Fatalf("not released: %+v", nb.Status)
	}
	if staged(h) || h.reboot.n != 0 {
		t.Fatalf("released node still staged or rebooted: staged=%v reboots=%d", staged(h), h.reboot.n)
	}
}

// Released after the reboot was already requested: the node comes back with a
// new boot ID and no target, and that is not a failed update.
func TestReleaseWhileRebootingIsNotAFailure(t *testing.T) {
	tg := target(rpiv1.ChipBCM2711, bo("sd", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2711, tg)
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash(); nb.Spec.RebootHash = tg.Hash() })
	if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhaseRebooting {
		t.Fatalf("setup: phase=%s", nb.Status.Phase)
	}
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec = rpiv1.NodeBootloaderSpec{} })
	h.info.BootID = "boot-2"
	nb := h.reconcile()
	if nb.Status.Phase != rpiv1.PhaseUnmanaged || nb.Status.FailedHash != "" || nb.Status.RebootingFromBootID != "" {
		t.Fatalf("phase=%s failedHash=%q", nb.Status.Phase, nb.Status.FailedHash)
	}
}

// A policy selecting an unmanaged node starts it from Pending; a sticky failure
// does not outlive the release.
func TestClaimAfterReleaseStartsClean(t *testing.T) {
	tg := target(rpiv1.ChipBCM2712, bo("nvme", "sd", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2712, tg)
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash(); nb.Spec.RebootHash = tg.Hash() })
	h.reconcile() // Rebooting
	h.info.BootID = "boot-2"
	if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhaseFailed { // came back unchanged
		t.Fatalf("setup: phase=%s", nb.Status.Phase)
	}
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec = rpiv1.NodeBootloaderSpec{} })
	if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhaseUnmanaged || nb.Status.FailedHash != "" {
		t.Fatalf("release did not clear the failure: %+v", nb.Status)
	}
	// Claimed again by the same target: not stuck Failed.
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec = rpiv1.NodeBootloaderSpec{PolicyName: "p", Target: &tg} })
	if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhasePending {
		t.Fatalf("after re-claim: phase=%s", nb.Status.Phase)
	}
}

func TestBootstrapCreatesTheObjectOnce(t *testing.T) {
	h := bareHarness(t, rpiv1.ChipBCM2711, true)
	if h.exists() {
		t.Fatal("setup: object already exists")
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := h.r.Bootstrap(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	nb := h.get()
	if nb.Spec.PolicyName != "" || nb.Spec.Target != nil || nb.Spec.ApprovedHash != "" {
		t.Fatalf("the bootstrapped object is not empty: %+v", nb.Spec)
	}
	// Owned by the Node, so it goes when the node does.
	if len(nb.OwnerReferences) != 1 || nb.OwnerReferences[0].Kind != "Node" || nb.OwnerReferences[0].Name != nodeName {
		t.Errorf("owner references = %+v", nb.OwnerReferences)
	}
}

func TestBootstrapWithoutANodeStillCreatesTheObject(t *testing.T) {
	h := bareHarness(t, rpiv1.ChipBCM2712, false)
	if err := h.r.Bootstrap(context.Background()); err != nil || !h.exists() {
		t.Fatalf("err=%v exists=%v", err, h.exists())
	}
	if len(h.get().OwnerReferences) != 0 {
		t.Error("an owner reference was invented for a node that could not be read")
	}
}

// An arm64 server (or any board the operator cannot manage) gets no object.
func TestBootstrapSkipsBoardsItCannotManage(t *testing.T) {
	h := bareHarness(t, rpiv1.ChipBCM2711, true)
	h.r.Probe = func() (node.Info, error) { return node.Info{}, errors.New("unsupported board") }
	if err := h.r.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.exists() {
		t.Fatal("created a NodeBootloader for a node that is not a supported Pi")
	}
	// And a reconcile for it creates nothing either.
	if _, err := h.r.Reconcile(context.Background(), reqFor(nodeName)); err != nil || h.exists() {
		t.Fatalf("reconcile: err=%v exists=%v", err, h.exists())
	}
}

// If someone deletes the object (or a controller bug does), the node reappears.
func TestDeletedObjectIsRecreated(t *testing.T) {
	h := bareHarness(t, rpiv1.ChipBCM2711, true)
	_ = h.r.Bootstrap(context.Background())
	h.reconcile()
	nb := h.get()
	if err := h.c.Delete(context.Background(), &nb); err != nil {
		t.Fatal(err)
	}
	if h.exists() {
		t.Fatal("setup: still exists")
	}
	if _, err := h.r.Reconcile(context.Background(), reqFor(nodeName)); err != nil {
		t.Fatal(err)
	}
	if !h.exists() {
		t.Fatal("the NodeBootloader was not recreated")
	}
}
