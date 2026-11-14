package eeprom

import "testing"

func TestParseBootOrder(t *testing.T) {
	o, err := ParseBootOrder("0xF416")
	if err != nil {
		t.Fatal(err)
	}
	want := BootOrder{ModeNVMe, ModeSD, ModeUSBMSD, ModeRestart}
	if len(o) != len(want) {
		t.Fatalf("got %v", o)
	}
	for i := range want {
		if o[i] != want[i] {
			t.Fatalf("got %v want %v", o, want)
		}
	}
	if o.Canonical() != "0xf416" {
		t.Errorf("Canonical = %s", o.Canonical())
	}
	for _, bad := range []string{"", "f416", "0x", "0xg1", "0x123456789", "0x9", "0x0", "0x8"} {
		if _, err := ParseBootOrder(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestBootOrderValidate(t *testing.T) {
	nvme := []BootMode{ModeNVMe}
	sd := []BootMode{ModeSD}
	usb := []BootMode{ModeUSBMSD, ModeBCMUSBMSD}
	cases := []struct {
		name    string
		order   string
		current []BootMode
		wantErr bool
	}{
		{"nvme first, running from nvme", "0xf416", nvme, false},
		{"sd last, running from sd", "0xf461", sd, false},
		{"usb accepted via either mode", "0xf15", usb, false},
		{"order drops the current device", "0xf41", nvme, true},
		{"stop before the current device", "0xf6e1", nvme, true},
		{"restart before the current device", "0x61f", nvme, true},
		{"stop only", "0xe", nil, true},
		{"restart only", "0xf", nil, true},
		{"rpiboot only is not bootable", "0xf3", nil, true},
		{"unknown current device, sane order", "0xf41", nil, false},
	}
	for _, c := range cases {
		o, err := ParseBootOrder(c.order)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if err := o.Validate(c.current); (err != nil) != c.wantErr {
			t.Errorf("%s: Validate(%s) err=%v wantErr=%v", c.name, c.order, err, c.wantErr)
		}
	}
}
