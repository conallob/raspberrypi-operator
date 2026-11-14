package node

import (
	"os"
	"path/filepath"
	"testing"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
	"github.com/conallob/raspberrypi-operator/internal/eeprom"
)

func TestChipFromCompatible(t *testing.T) {
	for in, want := range map[string]rpiv1.Chip{
		"raspberrypi,4-model-b\x00brcm,bcm2711\x00":        rpiv1.ChipBCM2711,
		"raspberrypi,4-compute-module\x00brcm,bcm2711\x00": rpiv1.ChipBCM2711,
		"raspberrypi,5-model-b\x00brcm,bcm2712\x00":        rpiv1.ChipBCM2712,
		"raspberrypi,5-compute-module\x00brcm,bcm2712\x00": rpiv1.ChipBCM2712,
		"raspberrypi,3-model-b\x00brcm,bcm2837\x00":        "",
		"": "",
	} {
		if got := ChipFromCompatible([]byte(in)); got != want {
			t.Errorf("ChipFromCompatible(%q) = %q, want %q", in, got, want)
		}
	}
	if SoC(rpiv1.ChipBCM2711) != "2711" || SoC(rpiv1.ChipBCM2712) != "2712" {
		t.Error("SoC names wrong")
	}
}

func TestBuildTimestampWidths(t *testing.T) {
	for in, want := range map[string]int64{
		"\x68\x1a\x2b\x3c":                 0x681a2b3c,
		"\x00\x00\x00\x01":                 1,
		"\x00\x00\x00\x00\x68\x1a\x2b\x3c": 0x681a2b3c,
		"":                                 0,
	} {
		if got := beInt([]byte(in)); got != want {
			t.Errorf("beInt(%x) = %d, want %d", in, got, want)
		}
	}
}

func TestModesFor(t *testing.T) {
	want := map[rpiv1.BootDevice][]eeprom.BootMode{
		rpiv1.BootDeviceSD:      {eeprom.ModeSD},
		rpiv1.BootDeviceNVMe:    {eeprom.ModeNVMe},
		rpiv1.BootDeviceUSB:     {eeprom.ModeUSBMSD, eeprom.ModeBCMUSBMSD},
		rpiv1.BootDeviceNetwork: {eeprom.ModeNetwork, eeprom.ModeHTTP},
	}
	for d, w := range want {
		got := ModesFor(d)
		if len(got) != len(w) {
			t.Errorf("%s: %v", d, got)
			continue
		}
		for i := range w {
			if got[i] != w[i] {
				t.Errorf("%s: %v want %v", d, got, w)
			}
		}
	}
	if ModesFor(rpiv1.BootDeviceUnknown) != nil {
		t.Error("an unknown device must yield no modes, so the order cannot be checked")
	}
}

// The boot partition is found by the longest matching mount point, so a bind
// mount inside / wins over /, and a sibling mount sharing a prefix does not.
func TestBootDeviceUsesTheLongestMatchingMount(t *testing.T) {
	root := t.TempDir()
	mi := filepath.Join(root, "mountinfo")
	content := "" +
		"1 0 8:1 / / rw - ext4 /dev/mmcblk0p2 rw\n" +
		"2 1 259:1 / /boot/firmware rw - vfat /dev/nvme0n1p1 rw\n" +
		"3 1 8:2 / /boot/firmware-old rw - vfat /dev/sda1 rw\n" +
		"4 1 0:5 / /data rw - nfs4 10.0.0.1:/export rw\n"
	if err := os.WriteFile(mi, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for bootfs, want := range map[string]rpiv1.BootDevice{
		"/boot/firmware":     rpiv1.BootDeviceNVMe, // not the / mount, not firmware-old
		"/boot/firmware-old": rpiv1.BootDeviceUSB,
		"/boot/firmware/sub": rpiv1.BootDeviceNVMe,
		"/etc":               rpiv1.BootDeviceSD, // falls back to /
		"/data":              rpiv1.BootDeviceNetwork,
	} {
		p := Prober{Root: root, BootFS: bootfs, MountInfo: mi}
		if got := p.bootDevice(); got != want {
			t.Errorf("bootDevice(%s) = %s, want %s", bootfs, got, want)
		}
	}
	if got := (Prober{Root: root, BootFS: "/x", MountInfo: filepath.Join(root, "missing")}).bootDevice(); got != rpiv1.BootDeviceUnknown {
		t.Errorf("unreadable mountinfo = %s, want Unknown", got)
	}
}

// /dev/disk/by-* style sources are resolved before being classified.
func TestBootDeviceFollowsDeviceSymlinks(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "dev")
	if err := os.MkdirAll(filepath.Join(dev, "disk/by-uuid"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "nvme0n1p1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../nvme0n1p1", filepath.Join(dev, "disk/by-uuid/ABCD-1234")); err != nil {
		t.Fatal(err)
	}
	p := Prober{Root: root}
	if got := p.classify("vfat", "/dev/disk/by-uuid/ABCD-1234"); got != rpiv1.BootDeviceNVMe {
		t.Errorf("classify via symlink = %s, want NVMe", got)
	}
}

func TestProbeRequiresABootloaderTimestamp(t *testing.T) {
	root, p := fixture(t, "x")
	if err := os.Remove(filepath.Join(root, "proc/device-tree/chosen/bootloader/build-timestamp")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Probe(); err == nil {
		t.Fatal("probe succeeded without a bootloader build timestamp")
	}
	if _, err := (Prober{Root: t.TempDir()}).Probe(); err == nil {
		t.Fatal("probe succeeded with no device tree at all")
	}
}
