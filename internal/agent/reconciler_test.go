package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
	"github.com/conallob/raspberrypi-operator/internal/eeprom"
	"github.com/conallob/raspberrypi-operator/internal/node"
)

const nodeName = "pi-01"

// image builds a minimal valid image: Pi 4 images are 512 KiB, Pi 5 2 MiB.
func image(size int, conf string) []byte {
	const fileMagic, magic = 0x55aaf11f, 0x55aaf00f
	b := bytes.Repeat([]byte{0xff}, size)
	binary.BigEndian.PutUint32(b[0:], magic)
	binary.BigEndian.PutUint32(b[4:], 0x1000)
	const pad, padMagic = 0x1008, 0x55aafeef
	binary.BigEndian.PutUint32(b[pad:], padMagic)
	binary.BigEndian.PutUint32(b[pad+4:], 0x20000-pad-8)
	off := 0x20000
	binary.BigEndian.PutUint32(b[off:], fileMagic)
	binary.BigEndian.PutUint32(b[off+4:], uint32(len(conf)+16))
	copy(b[off+8:off+20], append([]byte("bootconf.txt"), 0))
	copy(b[off+24:], conf)
	return b
}

type fakeFetcher struct {
	data  []byte
	err   error
	calls int
}

func (f *fakeFetcher) Fetch(context.Context, string, string) ([]byte, error) {
	f.calls++
	return f.data, f.err
}

type fakeRebooter struct{ n int }

func (f *fakeRebooter) Reboot() error { f.n++; return nil }

type harness struct {
	t      *testing.T
	c      client.Client
	r      *Reconciler
	info   node.Info
	fetch  *fakeFetcher
	reboot *fakeRebooter
	bootfs string
	now    time.Time
}

func newHarness(t *testing.T, chip rpiv1.Chip, target rpiv1.TargetSpec) *harness {
	t.Helper()
	sch := runtime.NewScheme()
	if err := rpiv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	nb := &rpiv1.NodeBootloader{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec:       rpiv1.NodeBootloaderSpec{PolicyName: "p", Target: &target},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(nb).WithStatusSubresource(nb).Build()
	size := 512 * 1024
	if chip == rpiv1.ChipBCM2712 {
		size = 2 * 1024 * 1024
	}
	h := &harness{
		t: t, c: c, bootfs: t.TempDir(), now: time.Unix(1_800_000_000, 0),
		fetch:  &fakeFetcher{data: image(size, "[all]\nBOOT_UART=1\n")},
		reboot: &fakeRebooter{},
		info: node.Info{
			Chip: chip, BuildTimestamp: node.SelfUpdateMinBuild + 1000, BootID: "boot-1",
			BootDevice: rpiv1.BootDeviceSD, Config: "[all]\nBOOT_UART=1\nBOOT_ORDER=0xf41\n",
		},
	}
	h.r = &Reconciler{
		Client: c, NodeName: nodeName, BootFS: h.bootfs, Fetch: h.fetch, Rebooter: h.reboot,
		Probe:    func() (node.Info, error) { return h.info, nil },
		Now:      func() time.Time { return h.now },
		Interval: time.Second,
	}
	// As in production, the agent evaluates a new object (it reports Pending)
	// before the controller approves anything.
	h.reconcile()
	return h
}

func (h *harness) get() rpiv1.NodeBootloader {
	var nb rpiv1.NodeBootloader
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: nodeName}, &nb); err != nil {
		h.t.Fatal(err)
	}
	return nb
}

func (h *harness) update(f func(*rpiv1.NodeBootloader)) {
	nb := h.get()
	f(&nb)
	if err := h.c.Update(context.Background(), &nb); err != nil {
		h.t.Fatal(err)
	}
}

// reconcile runs one reconcile and fails the test if the node's phase moved in a
// way the state machine (api/v1alpha1/phases.go) does not allow, so every agent
// test also checks every transition it causes.
func (h *harness) reconcile() rpiv1.NodeBootloader {
	h.t.Helper()
	from := h.get().Status.Phase
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: nodeName}}); err != nil {
		h.t.Fatal(err)
	}
	nb := h.get()
	if !rpiv1.CanTransition(from, nb.Status.Phase) {
		h.t.Errorf("phase went %q -> %q, which is not in the state machine", from, nb.Status.Phase)
	}
	return nb
}

// bo builds a boot order from API names.
func bo(names ...string) []rpiv1.BootSource {
	out := make([]rpiv1.BootSource, len(names))
	for i, n := range names {
		out[i] = rpiv1.BootSource(n)
	}
	return out
}

func target(chip rpiv1.Chip, order []rpiv1.BootSource) rpiv1.TargetSpec {
	on := true
	return rpiv1.TargetSpec{
		Firmware: rpiv1.FirmwareSpec{
			Images: []rpiv1.ImageSource{{Chip: chip, URL: "https://example.invalid/pieeprom.bin", SHA256: strings.Repeat("a", 64),
				BuildTimestamp: node.SelfUpdateMinBuild + 2000}},
		},
		Config: rpiv1.BootloaderConfig{BootOrder: order, PowerOffOnHalt: &on},
	}
}

func staged(h *harness) bool {
	_, err := os.Stat(filepath.Join(h.bootfs, node.UpdateFile))
	return err == nil
}

// TestFullUpdateBothChips walks Pending -> Staged -> Rebooting -> UpToDate on
// a Pi 4-class and a Pi 5-class node.
func TestFullUpdateBothChips(t *testing.T) {
	for _, chip := range []rpiv1.Chip{rpiv1.ChipBCM2711, rpiv1.ChipBCM2712} {
		t.Run(string(chip), func(t *testing.T) {
			tg := target(chip, bo("sd", "nvme", "usb", "restart"))
			h := newHarness(t, chip, tg)
			hash := tg.Hash()

			nb := h.reconcile()
			if nb.Status.Phase != rpiv1.PhasePending || staged(h) {
				t.Fatalf("unapproved: phase=%s staged=%v", nb.Status.Phase, staged(h))
			}
			if nb.Status.Chip != chip || nb.Status.BootOrder != "sd,usb,restart" || h.fetch.calls != 0 {
				t.Fatalf("status not populated / downloaded early: %+v calls=%d", nb.Status, h.fetch.calls)
			}

			h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = hash })
			nb = h.reconcile()
			if nb.Status.Phase != rpiv1.PhaseStaged || !staged(h) || nb.Status.StagedHash != hash {
				t.Fatalf("approved: phase=%s staged=%v msg=%s", nb.Status.Phase, staged(h), nb.Status.Message)
			}
			if h.reboot.n != 0 {
				t.Fatal("rebooted without authorisation")
			}
			// The staged image carries the merged config: unmanaged BOOT_UART kept.
			raw, _ := os.ReadFile(filepath.Join(h.bootfs, node.UpdateFile))
			parsed, err := eeprom.ParseImage(raw)
			if err != nil {
				t.Fatal(err)
			}
			conf, _ := parsed.Config()
			for _, want := range []string{"BOOT_UART=1", "BOOT_ORDER=0xf461", "POWER_OFF_ON_HALT=1"} {
				if !strings.Contains(conf, want) {
					t.Errorf("staged config missing %q:\n%s", want, conf)
				}
			}
			sig, _ := os.ReadFile(filepath.Join(h.bootfs, node.SigFile))
			if !strings.Contains(string(sig), "target-soc: "+node.SoC(chip)) {
				t.Errorf("sig has wrong SoC: %s", sig)
			}

			// A reconcile with nothing new does not re-download.
			h.reconcile()
			if h.fetch.calls != 1 {
				t.Errorf("redownloaded: %d fetches", h.fetch.calls)
			}

			h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.RebootHash = hash })
			nb = h.reconcile()
			if nb.Status.Phase != rpiv1.PhaseRebooting || h.reboot.n != 1 || nb.Status.RebootingFromBootID != "boot-1" {
				t.Fatalf("reboot: phase=%s reboots=%d", nb.Status.Phase, h.reboot.n)
			}

			// The node comes back with the new bootloader and config.
			h.info.BootID = "boot-2"
			h.info.BuildTimestamp = tg.Firmware.Images[0].BuildTimestamp
			h.info.Config = conf
			nb = h.reconcile()
			if nb.Status.Phase != rpiv1.PhaseUpToDate || staged(h) || nb.Status.RebootingFromBootID != "" {
				t.Fatalf("after reboot: phase=%s staged=%v msg=%s", nb.Status.Phase, staged(h), nb.Status.Message)
			}
			if h.reboot.n != 1 {
				t.Error("rebooted again after success")
			}
		})
	}
}

func TestConfigOnlyChange(t *testing.T) {
	tg := target(rpiv1.ChipBCM2712, bo("nvme", "sd", "usb", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2712, tg)
	h.info.BuildTimestamp = tg.Firmware.Images[0].BuildTimestamp // firmware already current
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash() })
	if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhaseStaged {
		t.Fatalf("config drift alone must stage an update: %s %s", nb.Status.Phase, nb.Status.Message)
	}
}

func TestUpToDate(t *testing.T) {
	tg := target(rpiv1.ChipBCM2711, bo("sd", "usb", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2711, tg)
	h.info.BuildTimestamp = tg.Firmware.Images[0].BuildTimestamp
	h.info.Config = "BOOT_ORDER=0xF41\nPOWER_OFF_ON_HALT=1\n"
	if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhaseUpToDate {
		t.Fatalf("phase = %s (%s)", nb.Status.Phase, nb.Status.Message)
	}
}

func TestBlocked(t *testing.T) {
	cases := []struct {
		name   string
		order  []rpiv1.BootSource
		mutate func(*node.Info, *rpiv1.TargetSpec)
		want   string
	}{
		{"order drops the boot device", bo("sd", "usb", "restart"), func(i *node.Info, _ *rpiv1.TargetSpec) { i.BootDevice = rpiv1.BootDeviceNVMe }, "cannot boot from the current boot device"},
		{"boot device unknown", bo("sd", "usb", "restart"), func(i *node.Info, _ *rpiv1.TargetSpec) { i.BootDevice = rpiv1.BootDeviceUnknown }, "cannot tell which device"},
		{"downgrade", bo("sd", "usb", "restart"), func(i *node.Info, t *rpiv1.TargetSpec) { i.BuildTimestamp = t.Firmware.Images[0].BuildTimestamp + 1 }, "older than the running build"},
		{"bootloader too old", bo("sd", "usb", "restart"), func(i *node.Info, _ *rpiv1.TargetSpec) { i.BuildTimestamp = 1 }, "too old for self-update"},
		{"config unreadable", bo("sd", "usb", "restart"), func(i *node.Info, _ *rpiv1.TargetSpec) { i.ConfigErr = node.ErrConfigUnavailable }, "blconfig"},
		{"no image for chip", bo("sd", "usb", "restart"), func(_ *node.Info, t *rpiv1.TargetSpec) { t.Firmware.Images[0].Chip = rpiv1.ChipBCM2712 }, "no firmware image"},
		{"conditional conflict", bo("sd", "usb", "restart"), func(i *node.Info, _ *rpiv1.TargetSpec) { i.Config = "[pi4]\nBOOT_ORDER=0x1\n" }, "conditional"},
		{"BOOT_ORDER in settings", bo("sd", "usb", "restart"), func(_ *node.Info, t *rpiv1.TargetSpec) {
			t.Config.Settings = map[string]rpiv1.SettingValue{"BOOT_ORDER": "0x1"}
		}, "config.bootOrder"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tg := target(rpiv1.ChipBCM2711, c.order)
			h := newHarness(t, rpiv1.ChipBCM2711, tg)
			c.mutate(&h.info, &tg)
			h.update(func(nb *rpiv1.NodeBootloader) {
				nb.Spec.Target = &tg
				nb.Spec.ApprovedHash = tg.Hash()
				nb.Spec.RebootHash = tg.Hash()
			})
			nb := h.reconcile()
			if nb.Status.Phase != rpiv1.PhaseBlocked || !strings.Contains(nb.Status.Message, c.want) {
				t.Fatalf("phase=%s msg=%q, want Blocked containing %q", nb.Status.Phase, nb.Status.Message, c.want)
			}
			if staged(h) || h.reboot.n != 0 {
				t.Fatal("a blocked node must not stage or reboot")
			}
		})
	}
}

func TestUnsafeBootOrderOverride(t *testing.T) {
	tg := target(rpiv1.ChipBCM2712, bo("sd", "usb", "restart")) // no NVMe, yet the node boots from NVMe
	tg.Safety.AllowUnsafeBootOrder = true
	h := newHarness(t, rpiv1.ChipBCM2712, tg)
	h.info.BootDevice = rpiv1.BootDeviceNVMe
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash() })
	if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhaseStaged {
		t.Fatalf("phase=%s msg=%s", nb.Status.Phase, nb.Status.Message)
	}
}

func TestFailedAfterRebootIsSticky(t *testing.T) {
	tg := target(rpiv1.ChipBCM2712, bo("sd", "nvme", "usb", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2712, tg)
	hash := tg.Hash()
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = hash; nb.Spec.RebootHash = hash })
	h.reconcile() // stages and reboots
	if h.reboot.n != 1 {
		t.Fatalf("reboots = %d", h.reboot.n)
	}
	// Comes back unchanged (the bootloader ignored the update).
	h.info.BootID = "boot-2"
	nb := h.reconcile()
	if nb.Status.Phase != rpiv1.PhaseFailed || nb.Status.FailedHash != hash || staged(h) {
		t.Fatalf("phase=%s failedHash=%q staged=%v", nb.Status.Phase, nb.Status.FailedHash, staged(h))
	}
	// Further reconciles must neither restage nor reboot (no reboot loop).
	for i := 0; i < 3; i++ {
		nb = h.reconcile()
	}
	if nb.Status.Phase != rpiv1.PhaseFailed || h.reboot.n != 1 || staged(h) {
		t.Fatalf("not sticky: phase=%s reboots=%d staged=%v", nb.Status.Phase, h.reboot.n, staged(h))
	}
	// A changed target clears the failure.
	tg2 := target(rpiv1.ChipBCM2712, bo("nvme", "sd", "usb", "restart"))
	h.update(func(nb *rpiv1.NodeBootloader) {
		nb.Spec.Target = &tg2
		nb.Spec.ApprovedHash = ""
		nb.Spec.RebootHash = ""
	})
	if nb = h.reconcile(); nb.Status.Phase != rpiv1.PhasePending {
		t.Fatalf("after retarget: %s", nb.Status.Phase)
	}
}

func TestRebootNeedsMatchingHash(t *testing.T) {
	tg := target(rpiv1.ChipBCM2711, bo("sd", "usb", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2711, tg)
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash(); nb.Spec.RebootHash = "stale" })
	if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhaseStaged || h.reboot.n != 0 {
		t.Fatalf("phase=%s reboots=%d: a stale reboot authorisation must not reboot", nb.Status.Phase, h.reboot.n)
	}
}

func TestTargetChangeUnstages(t *testing.T) {
	tg := target(rpiv1.ChipBCM2712, bo("sd", "nvme", "usb", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2712, tg)
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash() })
	h.reconcile()
	if !staged(h) {
		t.Fatal("expected staged")
	}
	tg2 := target(rpiv1.ChipBCM2712, bo("nvme", "sd", "usb", "restart"))
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.Target = &tg2 }) // approval is now stale
	nb := h.reconcile()
	if staged(h) || nb.Status.Phase != rpiv1.PhasePending || nb.Status.StagedHash != "" {
		t.Fatalf("stale staged update survived a retarget: staged=%v phase=%s", staged(h), nb.Status.Phase)
	}
}

func TestDownloadFailureBlocks(t *testing.T) {
	tg := target(rpiv1.ChipBCM2711, bo("sd", "usb", "restart"))
	h := newHarness(t, rpiv1.ChipBCM2711, tg)
	h.fetch.err = errors.New("sha256 mismatch")
	h.update(func(nb *rpiv1.NodeBootloader) { nb.Spec.ApprovedHash = tg.Hash(); nb.Spec.RebootHash = tg.Hash() })
	nb := h.reconcile()
	if nb.Status.Phase != rpiv1.PhaseBlocked || staged(h) || h.reboot.n != 0 {
		t.Fatalf("phase=%s staged=%v reboots=%d", nb.Status.Phase, staged(h), h.reboot.n)
	}
}

func TestIgnoresOtherNodes(t *testing.T) {
	h := newHarness(t, rpiv1.ChipBCM2711, target(rpiv1.ChipBCM2711, bo("sd", "usb", "restart")))
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
}

// TestMixedChipsShareOneTarget: the two SoC families build the same release at
// different times, so one policy must be able to say "up to date" for both.
func TestMixedChipsShareOneTarget(t *testing.T) {
	tg := rpiv1.TargetSpec{Firmware: rpiv1.FirmwareSpec{Images: []rpiv1.ImageSource{
		{Chip: rpiv1.ChipBCM2711, URL: "https://example.invalid/a", SHA256: strings.Repeat("a", 64), BuildTimestamp: 1793402435},
		{Chip: rpiv1.ChipBCM2712, URL: "https://example.invalid/b", SHA256: strings.Repeat("b", 64), BuildTimestamp: 1793393754},
	}}}
	for _, c := range []struct {
		chip rpiv1.Chip
		ts   int64
	}{{rpiv1.ChipBCM2711, 1793402435}, {rpiv1.ChipBCM2712, 1793393754}} {
		h := newHarness(t, c.chip, tg)
		h.info.BuildTimestamp = c.ts
		if nb := h.reconcile(); nb.Status.Phase != rpiv1.PhaseUpToDate {
			t.Errorf("%s at its own build: phase=%s msg=%s", c.chip, nb.Status.Phase, nb.Status.Message)
		}
	}
}

func TestTypedSettingsBecomeBootloaderKeys(t *testing.T) {
	yes, no := true, false
	tg := target(rpiv1.ChipBCM2712, bo("nvme", "usb", "sd", "restart"))
	tg.Config.BootUART = &no
	tg.Config.WakeOnGPIO = &yes
	tg.Config.Settings = map[string]rpiv1.SettingValue{"SD_BOOT_MAX_RETRIES": "3"}
	got, err := desiredSettings(&tg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"BOOT_ORDER": "0xf146", "BOOT_UART": "0", "WAKE_ON_GPIO": "1",
		"POWER_OFF_ON_HALT": "1", "SD_BOOT_MAX_RETRIES": "3",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestSettingsConflictsAreRefused(t *testing.T) {
	yes := true
	for name, mutate := range map[string]func(*rpiv1.TargetSpec){
		"typed and untyped": func(tg *rpiv1.TargetSpec) {
			tg.Config.Settings = map[string]rpiv1.SettingValue{"POWER_OFF_ON_HALT": "0"}
		},
		"BOOT_ORDER": func(tg *rpiv1.TargetSpec) { tg.Config.Settings = map[string]rpiv1.SettingValue{"BOOT_ORDER": "0x1"} },
		"self-update": func(tg *rpiv1.TargetSpec) {
			tg.Config.Settings = map[string]rpiv1.SettingValue{"ENABLE_SELF_UPDATE": "0"}
		},
		"unknown boot source": func(tg *rpiv1.TargetSpec) { tg.Config.BootOrder = bo("floppy") },
		"repeated source":     func(tg *rpiv1.TargetSpec) { tg.Config.BootOrder = bo("sd", "sd") },
	} {
		tg := target(rpiv1.ChipBCM2711, bo("sd", "restart"))
		tg.Config.PowerOffOnHalt = &yes
		mutate(&tg)
		if _, err := desiredSettings(&tg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func reqFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
}
