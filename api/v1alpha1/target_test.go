package v1alpha1_test

import (
	"testing"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
)

func baseTarget() rpiv1.TargetSpec {
	yes := true
	return rpiv1.TargetSpec{
		Firmware: rpiv1.FirmwareSpec{Images: []rpiv1.ImageSource{
			{Chip: rpiv1.ChipBCM2711, URL: "https://example.invalid/a", SHA256: "a", BuildTimestamp: 100},
			{Chip: rpiv1.ChipBCM2712, URL: "https://example.invalid/b", SHA256: "b", BuildTimestamp: 200},
		}},
		Config: rpiv1.BootloaderConfig{
			BootOrder:      []rpiv1.BootSource{rpiv1.BootNVMe, rpiv1.BootSD, rpiv1.BootRestart},
			PowerOffOnHalt: &yes,
			Settings:       map[string]rpiv1.SettingValue{"A": "1", "B": "2", "C": "3", "D": "4"},
		},
	}
}

// The hash binds approval and reboot authorisation to a target, so it must be
// stable for equal targets and change for every field that matters.
func TestHashIsStableAndSensitive(t *testing.T) {
	want := baseTarget().Hash()
	for i := 0; i < 50; i++ { // map iteration order is randomised
		if got := baseTarget().Hash(); got != want {
			t.Fatalf("hash is not stable: %s vs %s", got, want)
		}
	}
	no := false
	for name, mutate := range map[string]func(*rpiv1.TargetSpec){
		"image timestamp": func(s *rpiv1.TargetSpec) { s.Firmware.Images[0].BuildTimestamp++ },
		"image sha":       func(s *rpiv1.TargetSpec) { s.Firmware.Images[1].SHA256 = "c" },
		"image url":       func(s *rpiv1.TargetSpec) { s.Firmware.Images[0].URL = "https://example.invalid/z" },
		"boot order":      func(s *rpiv1.TargetSpec) { s.Config.BootOrder[0] = rpiv1.BootUSB },
		"boot order len":  func(s *rpiv1.TargetSpec) { s.Config.BootOrder = s.Config.BootOrder[:2] },
		"typed key":       func(s *rpiv1.TargetSpec) { s.Config.PowerOffOnHalt = &no },
		"typed key unset": func(s *rpiv1.TargetSpec) { s.Config.PowerOffOnHalt = nil },
		"setting value":   func(s *rpiv1.TargetSpec) { s.Config.Settings["A"] = "9" },
		"setting added":   func(s *rpiv1.TargetSpec) { s.Config.Settings["E"] = "5" },
		"allow downgrade": func(s *rpiv1.TargetSpec) { s.Safety.AllowDowngrade = true },
		"allow unsafe":    func(s *rpiv1.TargetSpec) { s.Safety.AllowUnsafeBootOrder = true },
	} {
		s := baseTarget()
		mutate(&s)
		if s.Hash() == want {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}

func TestImageFor(t *testing.T) {
	f := baseTarget().Firmware
	if img, ok := f.ImageFor(rpiv1.ChipBCM2712); !ok || img.BuildTimestamp != 200 {
		t.Errorf("bcm2712: %+v %v", img, ok)
	}
	f.Images = f.Images[:1]
	if _, ok := f.ImageFor(rpiv1.ChipBCM2712); ok {
		t.Error("found an image for a chip with none")
	}
}

// DeepCopy is generated, but a policy's target is copied into every
// NodeBootloader, so a shared map or slice would let one node's edit leak.
func TestDeepCopyDoesNotAlias(t *testing.T) {
	orig := baseTarget()
	cp := orig.DeepCopy()
	cp.Config.Settings["A"] = "changed"
	cp.Config.BootOrder[0] = rpiv1.BootHTTP
	cp.Firmware.Images[0].SHA256 = "changed"
	*cp.Config.PowerOffOnHalt = false
	if orig.Config.Settings["A"] != "1" || orig.Config.BootOrder[0] != rpiv1.BootNVMe ||
		orig.Firmware.Images[0].SHA256 != "a" || !*orig.Config.PowerOffOnHalt {
		t.Fatalf("copy shares memory with the original: %+v", orig)
	}
}
