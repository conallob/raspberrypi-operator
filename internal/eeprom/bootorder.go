package eeprom

import (
	"fmt"
	"strconv"
	"strings"
)

// BootMode is one hex digit of BOOT_ORDER.
type BootMode uint8

// Boot modes, from the BOOT_ORDER documentation at
// https://www.raspberrypi.com/documentation/computers/raspberry-pi.html#BOOT_ORDER
const (
	ModeSD        BootMode = 0x1
	ModeNetwork   BootMode = 0x2
	ModeRPIBoot   BootMode = 0x3
	ModeUSBMSD    BootMode = 0x4
	ModeBCMUSBMSD BootMode = 0x5
	ModeNVMe      BootMode = 0x6
	ModeHTTP      BootMode = 0x7
	ModeStop      BootMode = 0xe
	ModeRestart   BootMode = 0xf
)

func (m BootMode) String() string {
	switch m {
	case ModeSD:
		return "SD"
	case ModeNetwork:
		return "network"
	case ModeRPIBoot:
		return "rpiboot"
	case ModeUSBMSD:
		return "USB-MSD"
	case ModeBCMUSBMSD:
		return "BCM-USB-MSD"
	case ModeNVMe:
		return "NVMe"
	case ModeHTTP:
		return "HTTP"
	case ModeStop:
		return "stop"
	case ModeRestart:
		return "restart"
	}
	return fmt.Sprintf("%#x", uint8(m))
}

var modeNames = map[string]BootMode{
	"sd":      ModeSD,
	"network": ModeNetwork,
	"rpiboot": ModeRPIBoot,
	"usb":     ModeUSBMSD,
	"bcm-usb": ModeBCMUSBMSD,
	"nvme":    ModeNVMe,
	"http":    ModeHTTP,
	"stop":    ModeStop,
	"restart": ModeRestart,
}

// Name returns the lower-case name used in the BootloaderPolicy API.
func (m BootMode) Name() string {
	for n, v := range modeNames {
		if v == m {
			return n
		}
	}
	return fmt.Sprintf("%#x", uint8(m))
}

// BootOrderFromNames builds a BootOrder from API names, in the order the
// bootloader should try them. Unknown names, repeats and lists longer than the
// eight digits BOOT_ORDER can hold are rejected.
func BootOrderFromNames(names []string) (BootOrder, error) {
	if len(names) == 0 || len(names) > 8 {
		return nil, fmt.Errorf("bootOrder needs 1 to 8 entries, got %d", len(names))
	}
	seen := map[BootMode]bool{}
	o := make(BootOrder, 0, len(names))
	for _, n := range names {
		m, ok := modeNames[n]
		if !ok {
			return nil, fmt.Errorf("unknown boot source %q", n)
		}
		if seen[m] {
			return nil, fmt.Errorf("boot source %q is listed twice", n)
		}
		seen[m] = true
		o = append(o, m)
	}
	return o, nil
}

// Names returns the API names in the order they are tried.
func (o BootOrder) Names() []string {
	out := make([]string, len(o))
	for i, m := range o {
		out[i] = m.Name()
	}
	return out
}

// bootable reports whether the mode can load an OS.
func (m BootMode) bootable() bool {
	switch m {
	case ModeSD, ModeNetwork, ModeUSBMSD, ModeBCMUSBMSD, ModeNVMe, ModeHTTP:
		return true
	}
	return false
}

// BootOrder is a parsed BOOT_ORDER. Modes are in the order the bootloader
// tries them, which is right to left in the hex value.
type BootOrder []BootMode

// ParseBootOrder parses a value such as 0xf416.
func ParseBootOrder(s string) (BootOrder, error) {
	t := strings.TrimSpace(s)
	if len(t) < 3 || (t[:2] != "0x" && t[:2] != "0X") {
		return nil, fmt.Errorf("BOOT_ORDER %q must look like 0xf416", s)
	}
	digits := t[2:]
	if len(digits) > 8 {
		return nil, fmt.Errorf("BOOT_ORDER %q has more than 8 digits", s)
	}
	if _, err := strconv.ParseUint(digits, 16, 32); err != nil {
		return nil, fmt.Errorf("BOOT_ORDER %q is not hexadecimal", s)
	}
	var order BootOrder
	for i := len(digits) - 1; i >= 0; i-- {
		v, _ := strconv.ParseUint(digits[i:i+1], 16, 8)
		m := BootMode(v)
		switch m {
		case ModeSD, ModeNetwork, ModeRPIBoot, ModeUSBMSD, ModeBCMUSBMSD, ModeNVMe, ModeHTTP, ModeStop, ModeRestart:
			order = append(order, m)
		default:
			return nil, fmt.Errorf("BOOT_ORDER %q contains unsupported boot mode %#x", s, v)
		}
	}
	return order, nil
}

// Canonical returns the value in the lower-case 0x form.
func (o BootOrder) Canonical() string {
	var sb strings.Builder
	sb.WriteString("0x")
	for i := len(o) - 1; i >= 0; i-- {
		sb.WriteString(strconv.FormatUint(uint64(o[i]), 16))
	}
	return sb.String()
}

// Reaches reports whether the bootloader would try mode m before it hits a
// stop. A restart ends the walk too, because it begins again from the first
// entry and so never gets any further.
func (o BootOrder) Reaches(m ...BootMode) bool {
	for _, got := range o {
		if got == ModeStop || got == ModeRestart {
			return false
		}
		for _, want := range m {
			if got == want {
				return true
			}
		}
	}
	return false
}

// Validate checks that the order is internally sane and, when current is not
// empty, that it can still boot from the device the node is running from.
// current lists the modes that would load that device (e.g. NVMe -> ModeNVMe).
func (o BootOrder) Validate(current []BootMode) error {
	any := false
	for _, m := range o {
		if m == ModeStop || m == ModeRestart {
			break
		}
		if m.bootable() {
			any = true
			break
		}
	}
	if !any {
		return fmt.Errorf("BOOT_ORDER %s has no bootable device before a stop or restart", o.Canonical())
	}
	if len(current) > 0 && !o.Reaches(current...) {
		names := make([]string, len(current))
		for i, m := range current {
			names[i] = m.String()
		}
		return fmt.Errorf("BOOT_ORDER %s cannot boot from the current boot device (%s): the node would not come back",
			o.Canonical(), strings.Join(names, " or "))
	}
	return nil
}
