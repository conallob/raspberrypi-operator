package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
	"github.com/conallob/raspberrypi-operator/internal/node"
)

// A probe failure (e.g. not a Pi, device tree unreadable) must be reported, and
// must not stage or reboot anything.
func TestProbeFailureBlocks(t *testing.T) {
	tg := target(rpiv1.ChipBCM2711, bo("sd", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2711, tg)
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash(); nb.Spec.RebootHash = tg.Hash() })
	h.r.Probe = func() (node.Info, error) { return node.Info{}, errors.New("reading device tree: no such file") }
	nb := h.reconcile()
	if nb.Status.Phase != rpiv1.PhaseBlocked || nb.Status.Message == "" || staged(h) || h.reboot.n != 0 {
		t.Fatalf("phase=%s msg=%q staged=%v reboots=%d", nb.Status.Phase, nb.Status.Message, staged(h), h.reboot.n)
	}
}

// If the staged files are damaged between staging and reboot, the agent must
// notice and restage rather than reboot into whatever is on disk.
func TestDamagedStagedUpdateIsRestaged(t *testing.T) {
	tg := target(rpiv1.ChipBCM2712, bo("nvme", "sd", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2712, tg)
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash() })
	h.reconcile()
	if h.fetch.calls != 1 || !staged(h) {
		t.Fatal("expected one staged update")
	}
	if err := os.WriteFile(filepath.Join(h.bootfs, node.UpdateFile), []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.RebootHash = tg.Hash() })
	nb := h.reconcile()
	if h.fetch.calls != 2 {
		t.Fatalf("damaged update was not restaged (fetches=%d)", h.fetch.calls)
	}
	if _, ok, err := node.StagedDigest(h.bootfs); !ok || err != nil {
		t.Fatalf("restaged update is not intact: ok=%v err=%v", ok, err)
	}
	// It may reboot only after the restaged update has been verified.
	if nb.Status.Phase != rpiv1.PhaseRebooting || h.reboot.n != 1 {
		t.Fatalf("phase=%s reboots=%d", nb.Status.Phase, h.reboot.n)
	}
}

// Downloads that aren't valid EEPROM images are refused before anything is
// written.
func TestInvalidImageBlocks(t *testing.T) {
	for name, data := range map[string][]byte{
		"wrong size":  []byte("not an eeprom"),
		"no sections": make([]byte, 512*1024),
	} {
		t.Run(name, func(t *testing.T) {
			tg := target(rpiv1.ChipBCM2711, bo("sd", "restart"))
			h := newHarness(t, rpiv1.ChipBCM2711, tg)
			h.fetch.data = data
			h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash(); nb.Spec.RebootHash = tg.Hash() })
			nb := h.reconcile()
			if nb.Status.Phase != rpiv1.PhaseBlocked || staged(h) || h.reboot.n != 0 {
				t.Fatalf("phase=%s staged=%v reboots=%d", nb.Status.Phase, staged(h), h.reboot.n)
			}
		})
	}
}

// With nothing managed except firmware, an unreadable running config is fine:
// the image's own config is used.
func TestFirmwareOnlyUpdateToleratesUnreadableConfig(t *testing.T) {
	tg := target(rpiv1.ChipBCM2712, nil)
	tg.Config = rpiv1.BootloaderConfig{}
	h := newHarness(t, rpiv1.ChipBCM2712, tg)
	h.info.ConfigErr = node.ErrConfigUnavailable
	h.info.Config = ""
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash() })
	if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhaseStaged {
		t.Fatalf("phase=%s msg=%s", nb.Status.Phase, nb.Status.Message)
	}
}

func TestReconcileRequeuesAtTheInterval(t *testing.T) {
	h := newHarness(t, rpiv1.ChipBCM2711, target(rpiv1.ChipBCM2711, bo("sd", "restart")))
	h.r.Interval = 17 * time.Second
	res, err := h.r.Reconcile(t.Context(), reqFor(nodeName))
	if err != nil || res.RequeueAfter != 17*time.Second {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestDescribeBootOrder(t *testing.T) {
	for in, want := range map[string]string{
		"0xf146": "nvme,usb,sd,restart",
		"0XF41":  "sd,usb,restart",
		" 0xf1 ": "sd,restart",
		"0x8f":   "0x8f", // a mode the operator doesn't know is shown as-is
		"junk":   "junk",
	} {
		if got := describeBootOrder(in); got != want {
			t.Errorf("describeBootOrder(%q) = %q, want %q", in, got, want)
		}
	}
}
