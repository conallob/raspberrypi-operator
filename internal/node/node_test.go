package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
	"github.com/conallob/raspberrypi-operator/internal/eeprom"
)

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture lays out the kernel interfaces of a Pi 5 booted from NVMe.
func fixture(t *testing.T, config string) (root string, p Prober) {
	t.Helper()
	root = t.TempDir()
	write(t, filepath.Join(root, "proc/device-tree/compatible"), []byte("raspberrypi,5-model-b\x00brcm,bcm2712\x00"))
	write(t, filepath.Join(root, "proc/device-tree/model"), []byte("Raspberry Pi 5 Model B Rev 1.0\x00"))
	write(t, filepath.Join(root, "proc/device-tree/chosen/bootloader/build-timestamp"), []byte{0x68, 0x1a, 0x2b, 0x3c})
	write(t, filepath.Join(root, "proc/sys/kernel/random/boot_id"), []byte("boot-1\n"))

	// blconfig alias -> DT node -> nvmem device whose of_node links to it.
	dtBase := filepath.Join(root, "sys/firmware/devicetree/base")
	write(t, filepath.Join(dtBase, "aliases/blconfig"), []byte("/axi/blconfig\x00"))
	if err := os.MkdirAll(filepath.Join(dtBase, "axi/blconfig"), 0o755); err != nil {
		t.Fatal(err)
	}
	nv := filepath.Join(root, "sys/bus/nvmem/devices/rmem0")
	if err := os.MkdirAll(nv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dtBase, "axi/blconfig"), filepath.Join(nv, "of_node")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(nv, "nvmem"), []byte(config+"\x00\x00\xff\xff"))

	bootfs := filepath.Join(root, "boot/firmware")
	if err := os.MkdirAll(bootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	mi := "22 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw\n" +
		"30 22 259:1 / " + bootfs + " rw - vfat /dev/nvme0n1p1 rw\n"
	write(t, filepath.Join(root, "proc/self/mountinfo"), []byte(mi))
	return root, Prober{Root: root, BootFS: bootfs}
}

func TestProbe(t *testing.T) {
	_, p := fixture(t, "[all]\nBOOT_ORDER=0xf416\n")
	in, err := p.Probe()
	if err != nil {
		t.Fatal(err)
	}
	if in.Chip != rpiv1.ChipBCM2712 || in.BuildTimestamp != 0x681a2b3c || in.BootID != "boot-1" {
		t.Errorf("unexpected info: %+v", in)
	}
	if in.BootDevice != rpiv1.BootDeviceNVMe {
		t.Errorf("BootDevice = %v, want NVMe", in.BootDevice)
	}
	if in.ConfigErr != nil || in.Config != "[all]\nBOOT_ORDER=0xf416\n" {
		t.Errorf("config = %q err=%v", in.Config, in.ConfigErr)
	}
	if !strings.Contains(in.Model, "Raspberry Pi 5") {
		t.Errorf("model = %q", in.Model)
	}
}

func TestProbeConfigUnavailable(t *testing.T) {
	root, p := fixture(t, "x")
	if err := os.Remove(filepath.Join(root, "sys/firmware/devicetree/base/aliases/blconfig")); err != nil {
		t.Fatal(err)
	}
	in, err := p.Probe()
	if err != nil {
		t.Fatal(err)
	}
	if in.ConfigErr != ErrConfigUnavailable {
		t.Errorf("ConfigErr = %v", in.ConfigErr)
	}
}

func TestProbeUnsupportedBoard(t *testing.T) {
	root, p := fixture(t, "x")
	write(t, filepath.Join(root, "proc/device-tree/compatible"), []byte("raspberrypi,3-model-b\x00brcm,bcm2837\x00"))
	if _, err := p.Probe(); err == nil {
		t.Fatal("a Pi 3 must be rejected")
	}
}

func TestBootDeviceClassification(t *testing.T) {
	p := Prober{Root: t.TempDir()}
	cases := []struct {
		fstype, src string
		want        rpiv1.BootDevice
	}{
		{"vfat", "/dev/mmcblk0p1", rpiv1.BootDeviceSD},
		{"vfat", "/dev/nvme0n1p1", rpiv1.BootDeviceNVMe},
		{"vfat", "/dev/sda1", rpiv1.BootDeviceUSB},
		{"nfs4", "10.0.0.1:/export", rpiv1.BootDeviceNetwork},
		{"tmpfs", "tmpfs", rpiv1.BootDeviceUnknown},
		{"overlay", "overlay", rpiv1.BootDeviceUnknown},
		{"vfat", "/dev/loop3", rpiv1.BootDeviceUnknown},
	}
	for _, c := range cases {
		if got := p.classify(c.fstype, c.src); got != c.want {
			t.Errorf("classify(%s,%s) = %v want %v", c.fstype, c.src, got, c.want)
		}
	}
}

func TestMountInfoEscapes(t *testing.T) {
	if got := unescapeMountInfo(`/mnt/with\040space`); got != "/mnt/with space" {
		t.Errorf("got %q", got)
	}
}

func TestSelfUpdateBlocked(t *testing.T) {
	bootfs := t.TempDir()
	ok := Info{BuildTimestamp: SelfUpdateMinBuild + 1, Config: "BOOT_ORDER=0xf41\n"}
	if r := SelfUpdateBlocked(ok, bootfs); r != "" {
		t.Errorf("unexpected block: %s", r)
	}
	if r := SelfUpdateBlocked(Info{BuildTimestamp: 1}, bootfs); r == "" {
		t.Error("old bootloader not blocked")
	}
	if r := SelfUpdateBlocked(Info{BuildTimestamp: SelfUpdateMinBuild, Config: "ENABLE_SELF_UPDATE=0\n"}, bootfs); r == "" {
		t.Error("ENABLE_SELF_UPDATE=0 not blocked")
	}
	write(t, filepath.Join(bootfs, "config.txt"), []byte("arm_64bit=1\nbootloader_update = 0\n"))
	if r := SelfUpdateBlocked(ok, bootfs); r == "" {
		t.Error("bootloader_update=0 not blocked")
	}
}

func TestStageLifecycle(t *testing.T) {
	bootfs := t.TempDir()
	img := []byte("image-bytes")
	sig := eeprom.Sig(img, time.Unix(1700000000, 0), "2712")

	if _, ok, err := StagedDigest(bootfs); ok || err != nil {
		t.Fatalf("nothing staged yet: ok=%v err=%v", ok, err)
	}
	if err := Stage(bootfs, img, sig); err != nil {
		t.Fatal(err)
	}
	d, ok, err := StagedDigest(bootfs)
	if err != nil || !ok || len(d) != 64 {
		t.Fatalf("StagedDigest = %q %v %v", d, ok, err)
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(bootfs)
	if len(ents) != 2 {
		t.Errorf("expected exactly the .upd and .sig, got %d entries", len(ents))
	}
	// Restaging replaces the previous pair.
	img2 := []byte("other-image")
	if err := Stage(bootfs, img2, eeprom.Sig(img2, time.Now(), "2712")); err != nil {
		t.Fatal(err)
	}
	d2, _, _ := StagedDigest(bootfs)
	if d2 == d {
		t.Error("restage did not replace the staged image")
	}
	// Corruption is detected.
	write(t, filepath.Join(bootfs, UpdateFile), []byte("tampered"))
	if _, _, err := StagedDigest(bootfs); err == nil {
		t.Error("tampered image not detected")
	}
	if err := Unstage(bootfs); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := StagedDigest(bootfs); ok {
		t.Error("still staged after Unstage")
	}
	if err := Unstage(bootfs); err != nil {
		t.Errorf("Unstage must be idempotent: %v", err)
	}
}
