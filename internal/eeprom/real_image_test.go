package eeprom

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealImages runs the editor against genuine upstream images. It is skipped
// unless RPI_EEPROM_REPO points at a checkout of
// https://github.com/raspberrypi/rpi-eeprom, e.g.
//
//	RPI_EEPROM_REPO=/path/to/rpi-eeprom go test ./internal/eeprom -run RealImages -v
func TestRealImages(t *testing.T) {
	repo := os.Getenv("RPI_EEPROM_REPO")
	if repo == "" {
		t.Skip("RPI_EEPROM_REPO not set")
	}
	for _, dir := range []string{"firmware-2711", "firmware-2712"} {
		for _, release := range []string{"default", "latest"} {
			files, _ := filepath.Glob(filepath.Join(repo, dir, release, "pieeprom-*.bin"))
			for _, f := range files {
				name := dir + "/" + release + "/" + filepath.Base(f)
				t.Run(name, func(t *testing.T) { checkRealImage(t, f) })
			}
		}
	}
}

func checkRealImage(t *testing.T, path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	img, err := ParseImage(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf, err := img.Config()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	// The shipped config is a short [all] block; BOOT_ORDER is normally absent
	// and left to the bootloader's default.
	if !strings.Contains(conf, "[all]") {
		t.Fatalf("unexpected shipped config:\n%s", conf)
	}

	// Rewriting the config with itself must reproduce the image exactly: the
	// editor must not disturb any other byte.
	same, _ := ParseImage(raw)
	if err := same.SetConfig(conf); err != nil {
		t.Fatalf("SetConfig(own config): %v", err)
	}
	if !bytes.Equal(same.Bytes(), raw) {
		t.Errorf("identity rewrite changed the image (%d differing bytes)", diff(same.Bytes(), raw))
	}

	// A real change: only the config region may differ, and it must re-parse.
	edited, _ := ParseImage(raw)
	merged, err := ParseConfig(conf).Apply(map[string]string{"BOOT_ORDER": "0xf461", "POWER_OFF_ON_HALT": "1"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := edited.SetConfig(merged.String()); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	re, err := ParseImage(edited.Bytes())
	if err != nil {
		t.Fatalf("edited image does not re-parse: %v", err)
	}
	got, _ := re.Config()
	if !ParseConfig(got).Satisfied(map[string]string{"BOOT_ORDER": "0xf461", "POWER_OFF_ON_HALT": "1"}) {
		t.Errorf("edit not reflected:\n%s", got)
	}
	first, last := span(edited.Bytes(), raw)
	t.Logf("%d bytes differ, within [%#x, %#x] of %#x", diff(edited.Bytes(), raw), first, last, len(raw))
	if first < readOnlySize {
		t.Errorf("edit touched the read-only region at %#x", first)
	}
	if last >= len(raw)-eraseAlign {
		t.Errorf("edit touched the bootloader scratch page at %#x", last)
	}
}

func diff(a, b []byte) (n int) {
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

func span(a, b []byte) (first, last int) {
	first, last = -1, -1
	for i := range a {
		if a[i] != b[i] {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	return first, last
}
