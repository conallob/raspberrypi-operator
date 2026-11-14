package version

import (
	"strings"
	"testing"
)

func TestStringHasEveryField(t *testing.T) {
	old := [3]string{Version, Commit, Date}
	defer func() { Version, Commit, Date = old[0], old[1], old[2] }()

	Version, Commit, Date = "1.2.3", "abc1234", "2026-11-14T00:00:00Z"
	got := String()
	for _, want := range []string{"1.2.3", "abc1234", "2026-11-14T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}

// An unstamped build must say so, not claim to be a release.
func TestDefaultsIdentifyADevBuild(t *testing.T) {
	if Version != "dev" {
		t.Skipf("stamped at link time (%s)", Version)
	}
	if !strings.HasPrefix(String(), "dev ") {
		t.Errorf("String() = %q", String())
	}
}
