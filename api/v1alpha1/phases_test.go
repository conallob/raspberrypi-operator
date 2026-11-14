package v1alpha1_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
)

var allPhases = []rpiv1.Phase{
	rpiv1.PhaseUnmanaged, rpiv1.PhasePending, rpiv1.PhaseBlocked, rpiv1.PhaseStaged,
	rpiv1.PhaseRebooting, rpiv1.PhaseUpToDate, rpiv1.PhaseFailed,
}

func isPhase(p rpiv1.Phase) bool {
	for _, q := range allPhases {
		if p == q {
			return true
		}
	}
	return false
}

func TestStateMachineIsWellFormed(t *testing.T) {
	// Every phase has a row, and every target of every row is a real phase.
	for _, p := range append([]rpiv1.Phase{""}, allPhases...) {
		tos, ok := rpiv1.AllowedTransitions[p]
		if !ok {
			t.Errorf("phase %q has no row in AllowedTransitions", p)
		}
		seen := map[rpiv1.Phase]bool{}
		for _, to := range tos {
			if !isPhase(to) {
				t.Errorf("%q -> %q: not a phase", p, to)
			}
			if to == p {
				t.Errorf("%q lists itself; staying put is always allowed", p)
			}
			if seen[to] {
				t.Errorf("%q -> %q listed twice", p, to)
			}
			seen[to] = true
		}
	}
	if len(rpiv1.AllowedTransitions) != len(allPhases)+1 {
		t.Errorf("AllowedTransitions has rows for unknown phases")
	}
	// Every phase can be reached from a new object.
	reached := map[rpiv1.Phase]bool{"": true}
	for changed := true; changed; {
		changed = false
		for from := range reached {
			for _, to := range rpiv1.AllowedTransitions[from] {
				if !reached[to] {
					reached[to], changed = true, true
				}
			}
		}
	}
	for _, p := range allPhases {
		if !reached[p] {
			t.Errorf("phase %s is unreachable", p)
		}
	}
	// And every phase can be left (the machine has no dead end), and any node
	// can be released to Unmanaged by its policy going away.
	for _, p := range allPhases {
		if len(rpiv1.AllowedTransitions[p]) == 0 {
			t.Errorf("phase %s is a dead end", p)
		}
		if p != rpiv1.PhaseUnmanaged && !rpiv1.CanTransition(p, rpiv1.PhaseUnmanaged) {
			t.Errorf("a %s node cannot be released to Unmanaged", p)
		}
	}
}

func TestCanTransition(t *testing.T) {
	for _, p := range allPhases {
		if !rpiv1.CanTransition(p, p) {
			t.Errorf("staying in %s must always be allowed", p)
		}
	}
	for _, c := range []struct{ from, to rpiv1.Phase }{
		{rpiv1.PhaseUnmanaged, rpiv1.PhaseStaged}, // never staged without being selected and approved first
		{rpiv1.PhaseUnmanaged, rpiv1.PhaseRebooting},
		{rpiv1.PhaseUnmanaged, rpiv1.PhaseFailed},
		{rpiv1.PhaseUpToDate, rpiv1.PhaseFailed},
		{rpiv1.PhaseUpToDate, rpiv1.PhaseStaged},
	} {
		if rpiv1.CanTransition(c.from, c.to) {
			t.Errorf("%s -> %s must not be allowed", c.from, c.to)
		}
	}
	// The safety property in one line: nothing reaches Staged or Rebooting
	// without having been Pending (or Blocked) first, and an Unmanaged node can
	// do neither directly.
	for _, from := range []rpiv1.Phase{"", rpiv1.PhaseUnmanaged, rpiv1.PhaseUpToDate, rpiv1.PhaseFailed} {
		for _, to := range []rpiv1.Phase{rpiv1.PhaseStaged, rpiv1.PhaseRebooting} {
			if rpiv1.CanTransition(from, to) {
				t.Errorf("%q -> %s skips Pending", from, to)
			}
		}
	}
}

// docs/design.md carries the same table; it must not drift from the code.
func TestDesignDocTableMatchesTheCode(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "design.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	start, end := strings.Index(s, "<!-- phases:start -->"), strings.Index(s, "<!-- phases:end -->")
	if start < 0 || end < start {
		t.Fatal("docs/design.md has no phases table between its markers")
	}
	doc := map[rpiv1.Phase][]string{}
	for _, line := range strings.Split(s[start:end], "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) != 2 || strings.HasPrefix(strings.TrimSpace(cells[0]), "From") || strings.HasPrefix(strings.TrimSpace(cells[0]), "---") {
			continue
		}
		from := rpiv1.Phase(strings.Trim(strings.TrimSpace(cells[0]), "`"))
		if strings.Contains(string(from), "new object") {
			from = ""
		}
		var tos []string
		for _, c := range strings.Split(cells[1], ",") {
			tos = append(tos, strings.Trim(strings.TrimSpace(c), "`"))
		}
		sort.Strings(tos)
		doc[from] = tos
	}
	if len(doc) != len(rpiv1.AllowedTransitions) {
		t.Fatalf("doc has %d rows, code has %d", len(doc), len(rpiv1.AllowedTransitions))
	}
	for from, tos := range rpiv1.AllowedTransitions {
		var want []string
		for _, to := range tos {
			want = append(want, string(to))
		}
		sort.Strings(want)
		if strings.Join(doc[from], ",") != strings.Join(want, ",") {
			t.Errorf("from %q: doc says %v, code says %v", from, doc[from], want)
		}
	}
}
