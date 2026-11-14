// Package node reads the state of the Raspberry Pi it runs on and stages
// bootloader updates, using only the standard library and x/sys: no
// subprocesses.
package node

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
	"github.com/conallob/raspberrypi-operator/internal/eeprom"
)

// SelfUpdateMinBuild is the first bootloader build that can apply
// pieeprom.upd by itself (2022-04-26). Older bootloaders need recovery.bin on
// the SD card, which this operator does not manage.
const SelfUpdateMinBuild int64 = 1650968668

// ErrConfigUnavailable means the running bootloader's configuration could not
// be read without a subprocess (vcgencmd), so the node cannot be managed.
var ErrConfigUnavailable = errors.New("the bootloader config is not exposed via the blconfig nvmem device (needs a recent Raspberry Pi kernel)")

// Info is what the agent learns about the node.
type Info struct {
	Model          string
	Chip           rpiv1.Chip
	BuildTimestamp int64
	BootID         string
	BootDevice     rpiv1.BootDevice
	// Config is the running bootloader's bootconf.txt, empty if unreadable.
	Config    string
	ConfigErr error
}

// Prober inspects a node. Root is prefixed to every kernel interface path so
// tests can point it at a fixture tree; production uses "/".
type Prober struct {
	Root string
	// BootFS is the (container) path of the boot partition.
	BootFS string
	// MountInfo overrides the mountinfo file; default <Root>/proc/self/mountinfo.
	MountInfo string
}

func (p Prober) path(rel string) string { return filepath.Join(p.Root, rel) }

// Probe gathers Info. Only an unsupported board is an error; a failure to read
// the config is recorded in Info.ConfigErr so it can be reported as a block.
func (p Prober) Probe() (Info, error) {
	var in Info

	compat, err := os.ReadFile(p.path("proc/device-tree/compatible"))
	if err != nil {
		return in, fmt.Errorf("reading device tree (is this a Raspberry Pi?): %w", err)
	}
	in.Chip = ChipFromCompatible(compat)
	if in.Chip == "" {
		return in, fmt.Errorf("unsupported board (compatible=%q); need a BCM2711 or BCM2712 Raspberry Pi",
			strings.ReplaceAll(strings.TrimRight(string(compat), "\x00"), "\x00", ","))
	}
	if m, err := os.ReadFile(p.path("proc/device-tree/model")); err == nil {
		in.Model = strings.TrimRight(string(m), "\x00\n")
	}

	ts, err := os.ReadFile(p.path("proc/device-tree/chosen/bootloader/build-timestamp"))
	if err != nil {
		return in, fmt.Errorf("reading bootloader build timestamp: %w", err)
	}
	in.BuildTimestamp = beInt(ts)

	if id, err := os.ReadFile(p.path("proc/sys/kernel/random/boot_id")); err == nil {
		in.BootID = strings.TrimSpace(string(id))
	}

	in.BootDevice = p.bootDevice()
	in.Config, in.ConfigErr = p.bootloaderConfig()
	return in, nil
}

// ChipFromCompatible maps the device-tree "compatible" list to a SoC family.
func ChipFromCompatible(compat []byte) rpiv1.Chip {
	for _, c := range bytes.Split(compat, []byte{0}) {
		switch string(c) {
		case "brcm,bcm2711":
			return rpiv1.ChipBCM2711
		case "brcm,bcm2712":
			return rpiv1.ChipBCM2712
		}
	}
	return ""
}

// SoC returns the numeric SoC name used in pieeprom.sig, e.g. "2712".
func SoC(c rpiv1.Chip) string { return strings.TrimPrefix(string(c), "bcm") }

func beInt(b []byte) int64 {
	if len(b) > 8 {
		b = b[len(b)-8:]
	}
	var buf [8]byte
	copy(buf[8-len(b):], b)
	return int64(binary.BigEndian.Uint64(buf[:]))
}

// bootloaderConfig reads bootconf.txt from the nvmem device that the firmware
// exposes for the running bootloader (the same source rpi-eeprom-update
// prefers over vcgencmd).
func (p Prober) bootloaderConfig() (string, error) {
	base := p.path("sys/firmware/devicetree/base")
	alias, err := os.ReadFile(filepath.Join(base, "aliases/blconfig"))
	if err != nil {
		return "", ErrConfigUnavailable
	}
	want := filepath.Clean(filepath.Join(base, strings.TrimRight(string(alias), "\x00\n")))

	nvmemDir := p.path("sys/bus/nvmem/devices")
	ents, err := os.ReadDir(nvmemDir)
	if err != nil {
		return "", ErrConfigUnavailable
	}
	for _, e := range ents {
		of, err := filepath.EvalSymlinks(filepath.Join(nvmemDir, e.Name(), "of_node"))
		if err != nil || filepath.Clean(of) != want {
			continue
		}
		b, err := os.ReadFile(filepath.Join(nvmemDir, e.Name(), "nvmem"))
		if err != nil {
			return "", fmt.Errorf("reading bootloader config nvmem: %w", err)
		}
		return eeprom.TrimPadding(string(b)), nil
	}
	return "", ErrConfigUnavailable
}

// bootDevice classifies the device behind the boot partition.
func (p Prober) bootDevice() rpiv1.BootDevice {
	mi := p.MountInfo
	if mi == "" {
		mi = p.path("proc/self/mountinfo")
	}
	f, err := os.Open(mi)
	if err != nil {
		return rpiv1.BootDeviceUnknown
	}
	defer func() { _ = f.Close() }()
	fstype, source := mountFor(f, p.BootFS)
	return p.classify(fstype, source)
}

// mountFor finds the mount holding dir (longest mount point wins).
func mountFor(f *os.File, dir string) (fstype, source string) {
	dir = filepath.Clean(dir)
	best := -1
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		left, right, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		lf := strings.Fields(left)
		rf := strings.Fields(right)
		if len(lf) < 5 || len(rf) < 2 {
			continue
		}
		mp := unescapeMountInfo(lf[4])
		if dir != mp && !strings.HasPrefix(dir, strings.TrimSuffix(mp, "/")+"/") {
			continue
		}
		if len(mp) > best {
			best, fstype, source = len(mp), rf[0], unescapeMountInfo(rf[1])
		}
	}
	return fstype, source
}

// unescapeMountInfo decodes the \040-style octal escapes in mountinfo fields.
func unescapeMountInfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				sb.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

func (p Prober) classify(fstype, source string) rpiv1.BootDevice {
	switch fstype {
	case "nfs", "nfs4", "cifs":
		return rpiv1.BootDeviceNetwork
	}
	if !strings.HasPrefix(source, "/dev/") {
		return rpiv1.BootDeviceUnknown
	}
	// Resolve /dev/disk/by-* and /dev/root style names where possible.
	if r, err := filepath.EvalSymlinks(filepath.Join(p.Root, source)); err == nil {
		source = strings.TrimPrefix(r, filepath.Clean(p.Root))
	}
	name := filepath.Base(source)
	switch {
	case strings.HasPrefix(name, "mmcblk"):
		return rpiv1.BootDeviceSD // includes CM4 eMMC, which boots as "SD"
	case strings.HasPrefix(name, "nvme"):
		return rpiv1.BootDeviceNVMe
	case strings.HasPrefix(name, "sd"):
		return rpiv1.BootDeviceUSB
	}
	return rpiv1.BootDeviceUnknown
}

// ModesFor returns the BOOT_ORDER modes that would load the given device, or
// nil if it is unknown.
func ModesFor(d rpiv1.BootDevice) []eeprom.BootMode {
	switch d {
	case rpiv1.BootDeviceSD:
		return []eeprom.BootMode{eeprom.ModeSD}
	case rpiv1.BootDeviceUSB:
		return []eeprom.BootMode{eeprom.ModeUSBMSD, eeprom.ModeBCMUSBMSD}
	case rpiv1.BootDeviceNVMe:
		return []eeprom.BootMode{eeprom.ModeNVMe}
	case rpiv1.BootDeviceNetwork:
		return []eeprom.BootMode{eeprom.ModeNetwork, eeprom.ModeHTTP}
	}
	return nil
}

// SelfUpdateBlocked explains why a staged update would not be applied by the
// bootloader's self-update, or returns "" if it should be.
func SelfUpdateBlocked(in Info, bootfs string) string {
	if in.BuildTimestamp < SelfUpdateMinBuild {
		return fmt.Sprintf("running bootloader (build %d) is too old for self-update; update it once by hand with a recovery SD card", in.BuildTimestamp)
	}
	if v, ok := eeprom.ParseConfig(in.Config).Get("ENABLE_SELF_UPDATE"); ok && v == "0" {
		return "ENABLE_SELF_UPDATE=0 in the running bootloader config; self-update is disabled"
	}
	if b, err := os.ReadFile(filepath.Join(bootfs, "config.txt")); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			if strings.ReplaceAll(strings.TrimSpace(sc.Text()), " ", "") == "bootloader_update=0" {
				return "bootloader_update=0 in config.txt; self-update is disabled"
			}
		}
	}
	return ""
}
