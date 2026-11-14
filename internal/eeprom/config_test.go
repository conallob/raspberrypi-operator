package eeprom

import (
	"strings"
	"testing"
)

func TestConfigApply(t *testing.T) {
	in := "[all]\nBOOT_UART=1\n# keep me\nBOOT_ORDER=0xf41\n\n[gpio8=0]\nBOOT_UART=0\n"
	c := ParseConfig(in)

	out, err := c.Apply(map[string]string{"BOOT_ORDER": "0xF416", "POWER_OFF_ON_HALT": "1"})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"# keep me\n", "BOOT_UART=1\n", "BOOT_ORDER=0xf416\n", "POWER_OFF_ON_HALT=1\n", "[gpio8=0]\nBOOT_UART=0\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// New keys go before the first conditional section.
	if strings.Index(got, "POWER_OFF_ON_HALT") > strings.Index(got, "[gpio8=0]") {
		t.Errorf("new key landed inside the conditional section:\n%s", got)
	}
	// Original untouched.
	if c.String() != in {
		t.Error("Apply modified its receiver")
	}
	// Idempotent.
	again, err := out.Apply(map[string]string{"BOOT_ORDER": "0xf416", "POWER_OFF_ON_HALT": "1"})
	if err != nil || again.String() != got {
		t.Errorf("not idempotent: %v\n%s", err, again.String())
	}
}

func TestConfigConditionalConflict(t *testing.T) {
	c := ParseConfig("[all]\nBOOT_ORDER=0xf41\n[pi4]\nBOOT_ORDER=0xf1\n")
	if _, err := c.Apply(map[string]string{"BOOT_ORDER": "0xf416"}); err == nil {
		t.Fatal("expected a conflict error")
	}
}

func TestConfigSatisfied(t *testing.T) {
	c := ParseConfig("BOOT_ORDER=0xF416\nPOWER_OFF_ON_HALT=1\x00\x00\xff")
	if !c.Satisfied(map[string]string{"BOOT_ORDER": "0xf416", "POWER_OFF_ON_HALT": "1"}) {
		t.Error("should be satisfied (case-insensitive BOOT_ORDER, padding trimmed)")
	}
	if c.Satisfied(map[string]string{"BOOT_ORDER": "0xf461"}) {
		t.Error("different order must not be satisfied")
	}
	if c.Satisfied(map[string]string{"MISSING": "1"}) {
		t.Error("missing key must not be satisfied")
	}
	// A conditional-section value does not count as the global value.
	if ParseConfig("[pi4]\nBOOT_ORDER=0xf416\n").Satisfied(map[string]string{"BOOT_ORDER": "0xf416"}) {
		t.Error("conditional value counted as global")
	}
}

func TestConfigRejectsBadKeys(t *testing.T) {
	c := ParseConfig("")
	for _, k := range []string{"lower", "BAD KEY", "A=B", ""} {
		if _, err := c.Apply(map[string]string{k: "1"}); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	if _, err := c.Apply(map[string]string{"A": "1\nB=2"}); err == nil {
		t.Error("newline injection accepted")
	}
}

func TestTrimPaddingIsByteWise(t *testing.T) {
	// A genuine U+FFFD character is data, not padding.
	if got := TrimPadding("A=1\n\xef\xbf\xbd\x00\xff\xff"); got != "A=1\n\xef\xbf\xbd" {
		t.Errorf("got %q", got)
	}
	if got := TrimPadding("\xff\x00"); got != "" {
		t.Errorf("got %q", got)
	}
}
