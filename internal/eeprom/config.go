package eeprom

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// KeyBootOrder is the BOOT_ORDER key.
const KeyBootOrder = "BOOT_ORDER"

var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Config is a parsed bootloader configuration (bootconf.txt). It keeps every
// line, so rendering preserves comments, ordering and unmanaged settings.
type Config struct {
	lines []string
}

// ParseConfig parses bootconf.txt content. Trailing NUL or 0xff padding left
// by the EEPROM or nvmem layer is dropped.
func ParseConfig(s string) *Config {
	s = TrimPadding(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return &Config{}
	}
	return &Config{lines: strings.Split(s, "\n")}
}

// TrimPadding removes trailing NUL and 0xff bytes, the filler used by the
// EEPROM and the nvmem layer. It works on bytes: strings.TrimRight would read
// "\xff" in its cutset as the Unicode replacement character instead.
func TrimPadding(s string) string {
	end := len(s)
	for end > 0 && (s[end-1] == 0x00 || s[end-1] == 0xff) {
		end--
	}
	return s[:end]
}

// scope describes where in the file a line sits. Settings before any
// [section] header, or under [all], apply to every board; settings under any
// other header are conditional.
func isGlobalSection(header string) bool {
	return strings.EqualFold(header, "[all]")
}

func parseAssignment(line string) (key, val string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "[") {
		return "", "", false
	}
	k, v, found := strings.Cut(t, "=")
	if !found {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}

// Get returns the effective global value of key (the last assignment in the
// unconditional scope).
func (c *Config) Get(key string) (string, bool) {
	var val string
	var found bool
	global := true
	for _, l := range c.lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			global = isGlobalSection(t)
			continue
		}
		if k, v, ok := parseAssignment(l); ok && global && k == key {
			val, found = v, true
		}
	}
	return val, found
}

// Conditional reports which of keys also appear under a conditional section
// header. Such an assignment can override the global one on some boards, so
// the operator will not manage it.
func (c *Config) Conditional(keys []string) []string {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	var out []string
	seen := map[string]bool{}
	global := true
	for _, l := range c.lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			global = isGlobalSection(t)
			continue
		}
		if k, _, ok := parseAssignment(l); ok && !global && want[k] && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	return out
}

// Set replaces key's global assignment, or adds it before the first
// conditional section (or at the end if there is none).
func (c *Config) Set(key, val string) error {
	if !keyRe.MatchString(key) {
		return fmt.Errorf("invalid bootloader key %q", key)
	}
	if strings.ContainsAny(val, "\r\n\x00") {
		return fmt.Errorf("invalid value for %s: control characters", key)
	}
	line := key + "=" + val
	global := true
	last := -1
	insertAt := len(c.lines)
	for i, l := range c.lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			if global && !isGlobalSection(t) {
				insertAt = i
			}
			global = isGlobalSection(t)
			continue
		}
		if k, _, ok := parseAssignment(l); ok && global && k == key {
			last = i
		}
	}
	if last >= 0 {
		c.lines[last] = line
		return nil
	}
	c.lines = append(c.lines[:insertAt], append([]string{line}, c.lines[insertAt:]...)...)
	return nil
}

// String renders the configuration.
func (c *Config) String() string {
	if len(c.lines) == 0 {
		return ""
	}
	return strings.Join(c.lines, "\n") + "\n"
}

// Normalize canonicalises a value for comparison. BOOT_ORDER is compared as a
// number so 0xF416 and 0xf416 match; everything else is compared literally.
func Normalize(key, val string) string {
	val = strings.TrimSpace(val)
	if key == KeyBootOrder {
		if o, err := ParseBootOrder(val); err == nil {
			return o.Canonical()
		}
	}
	return val
}

// Satisfied reports whether every desired key already has its desired value.
func (c *Config) Satisfied(desired map[string]string) bool {
	for k, want := range desired {
		got, ok := c.Get(k)
		if !ok || Normalize(k, got) != Normalize(k, want) {
			return false
		}
	}
	return true
}

// Apply merges desired settings into a copy of c. It refuses if a managed key
// also appears under a conditional section, because that assignment could
// silently override ours.
func (c *Config) Apply(desired map[string]string) (*Config, error) {
	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	if cond := c.Conditional(keys); len(cond) > 0 {
		return nil, fmt.Errorf("refusing to manage %s: also set under a conditional section in the node's config",
			strings.Join(cond, ", "))
	}
	out := &Config{lines: append([]string(nil), c.lines...)}
	// Deterministic order for stable output.
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	for _, k := range sorted {
		if err := out.Set(k, Normalize(k, desired[k])); err != nil {
			return nil, err
		}
	}
	return out, nil
}
