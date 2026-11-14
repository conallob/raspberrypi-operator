package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Chip is the SoC family of a Raspberry Pi. It selects which EEPROM image a
// node uses.
// +kubebuilder:validation:Enum=bcm2711;bcm2712
type Chip string

const (
	// ChipBCM2711 covers Pi 4, Pi 400 and CM4.
	ChipBCM2711 Chip = "bcm2711"
	// ChipBCM2712 covers Pi 5, Pi 500 and CM5.
	ChipBCM2712 Chip = "bcm2712"
)

// ImageSource locates one EEPROM image (a pieeprom-*.bin file from
// https://github.com/raspberrypi/rpi-eeprom) for one SoC family.
type ImageSource struct {
	Chip Chip `json:"chip"`

	// URL is where the node agent downloads the image from. Must be https.
	// +kubebuilder:validation:Pattern=`^https://.+`
	URL string `json:"url"`

	// SHA256 is the hex digest of the image. The agent refuses to stage an
	// image that does not match, so pin it.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`

	// BuildTimestamp is the Unix time of this image's bootloader build, as the
	// running bootloader reports it in
	// /proc/device-tree/chosen/bootloader/build-timestamp (also "timestamp" in
	// `vcgencmd bootloader_version`). The two SoC families build the same
	// release at different times, so it is set per image. rpi-eeprom lists it
	// as build_epoch in firmware-2711/versions.txt and firmware-2712/versions.txt.
	// The agent uses it to decide whether a node already runs this image and to
	// verify the update after reboot.
	// +kubebuilder:validation:Minimum=1
	BuildTimestamp int64 `json:"buildTimestamp"`
}

// FirmwareSpec declares the bootloader build that nodes should run.
type FirmwareSpec struct {
	// Images holds one image per SoC family present in the selected nodes.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +listType=map
	// +listMapKey=chip
	Images []ImageSource `json:"images"`
}

// BootSource is one thing the bootloader can try to boot from, or a control
// step in the boot order.
// +kubebuilder:validation:Enum=sd;network;rpiboot;usb;bcm-usb;nvme;http;stop;restart
type BootSource string

// Boot sources. usb is USB mass storage; bcm-usb is the same via the BCM
// ROM's own USB boot. stop halts the boot sequence and restart begins it again.
const (
	BootSD      BootSource = "sd" // also CM4 eMMC
	BootNetwork BootSource = "network"
	BootRPIBoot BootSource = "rpiboot"
	BootUSB     BootSource = "usb"
	BootBCMUSB  BootSource = "bcm-usb"
	BootNVMe    BootSource = "nvme"
	BootHTTP    BootSource = "http"
	BootStop    BootSource = "stop"
	BootRestart BootSource = "restart"
)

// SettingValue is the value of a bootloader setting: printable ASCII, so no
// line breaks can smuggle in extra settings.
// +kubebuilder:validation:Pattern=`^[ -~]{0,64}$`
type SettingValue string

// BootloaderConfig is the desired bootloader configuration (the bootconf.txt
// embedded in the EEPROM image). Only keys named here are managed; every other
// key in the node's current configuration is preserved.
//
// +kubebuilder:validation:XValidation:rule="!has(self.settings) || !(has(self.bootUART) && 'BOOT_UART' in self.settings)",message="BOOT_UART is set both as bootUART and in settings"
// +kubebuilder:validation:XValidation:rule="!has(self.settings) || !(has(self.wakeOnGPIO) && 'WAKE_ON_GPIO' in self.settings)",message="WAKE_ON_GPIO is set both as wakeOnGPIO and in settings"
// +kubebuilder:validation:XValidation:rule="!has(self.settings) || !(has(self.powerOffOnHalt) && 'POWER_OFF_ON_HALT' in self.settings)",message="POWER_OFF_ON_HALT is set both as powerOffOnHalt and in settings"
// +kubebuilder:validation:XValidation:rule="!has(self.settings) || !(has(self.netInstallAtPowerOn) && 'NET_INSTALL_AT_POWER_ON' in self.settings)",message="NET_INSTALL_AT_POWER_ON is set both as netInstallAtPowerOn and in settings"
type BootloaderConfig struct {
	// BootOrder lists what the bootloader tries, first to last, e.g.
	// [nvme, usb, sd, restart]. It becomes BOOT_ORDER (the same list written as
	// hex digits, last-tried first). Include restart (or stop) last, or a node
	// with nothing bootable shows a diagnostic screen. The agent refuses an
	// order that cannot boot from the device the node is currently running
	// from, unless safety.allowUnsafeBootOrder is set.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:XValidation:rule="self.all(s, self.exists_one(t, t == s))",message="a boot source may be listed only once"
	// +kubebuilder:validation:XValidation:rule="self.exists(s, s in ['sd', 'network', 'usb', 'bcm-usb', 'nvme', 'http'])",message="bootOrder must include at least one bootable source"
	// +listType=atomic
	// +optional
	BootOrder []BootSource `json:"bootOrder,omitempty"`

	// BootUART enables the bootloader's UART console (BOOT_UART).
	// +optional
	BootUART *bool `json:"bootUART,omitempty"`
	// WakeOnGPIO lets a GPIO wake a halted board (WAKE_ON_GPIO).
	// +optional
	WakeOnGPIO *bool `json:"wakeOnGPIO,omitempty"`
	// PowerOffOnHalt powers the board off, not just halts it, on shutdown
	// (POWER_OFF_ON_HALT).
	// +optional
	PowerOffOnHalt *bool `json:"powerOffOnHalt,omitempty"`
	// NetInstallAtPowerOn enables network install at power on
	// (NET_INSTALL_AT_POWER_ON).
	// +optional
	NetInstallAtPowerOn *bool `json:"netInstallAtPowerOn,omitempty"`

	// Settings are any other bootloader keys, e.g. SD_BOOT_MAX_RETRIES: "3".
	// Keys named by a typed field above, BOOT_ORDER, and ENABLE_SELF_UPDATE
	// (which the update mechanism depends on) are rejected.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[A-Z][A-Z0-9_]*$'))",message="setting names are upper-case letters, digits and underscores"
	// +kubebuilder:validation:XValidation:rule="!('BOOT_ORDER' in self)",message="set BOOT_ORDER with bootOrder, not in settings"
	// +kubebuilder:validation:XValidation:rule="!('ENABLE_SELF_UPDATE' in self)",message="ENABLE_SELF_UPDATE cannot be managed: the update mechanism depends on it"
	// +optional
	Settings map[string]SettingValue `json:"settings,omitempty"`
}

// Safety relaxes the agent's pre-flight checks. Both default to false.
type Safety struct {
	// AllowDowngrade permits installing a bootloader older than the one that
	// is running.
	// +optional
	AllowDowngrade bool `json:"allowDowngrade,omitempty"`

	// AllowUnsafeBootOrder permits a BOOT_ORDER that cannot boot from the
	// device the node is running from, or whose current boot device cannot be
	// determined. A wrong value can leave a node unbootable until it is
	// recovered by hand.
	// +optional
	AllowUnsafeBootOrder bool `json:"allowUnsafeBootOrder,omitempty"`
}

// TargetSpec is the desired bootloader state of a node.
type TargetSpec struct {
	Firmware FirmwareSpec `json:"firmware"`
	// +optional
	Config BootloaderConfig `json:"config,omitempty"`
	// +optional
	Safety Safety `json:"safety,omitempty"`
}

// Hash returns a stable digest of the target. Approval and reboot
// authorisation are bound to this value, so changing the target invalidates
// them and the node must be approved again.
func (t TargetSpec) Hash() string {
	b, _ := json.Marshal(t) // map keys are emitted sorted, so this is stable
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// ImageFor returns the image configured for chip.
func (f *FirmwareSpec) ImageFor(chip Chip) (ImageSource, bool) {
	for _, i := range f.Images {
		if i.Chip == chip {
			return i, true
		}
	}
	return ImageSource{}, false
}
