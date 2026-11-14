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
	// +listType=map
	// +listMapKey=chip
	Images []ImageSource `json:"images"`
}

// BootloaderConfig is the desired bootloader configuration (the bootconf.txt
// embedded in the EEPROM image). Only keys named here are managed; every other
// key in the node's current configuration is preserved.
type BootloaderConfig struct {
	// BootOrder is the BOOT_ORDER value, e.g. 0xf416 (NVMe, USB, SD, then
	// restart). Read right to left, one hex digit per boot mode. The agent
	// refuses an order that cannot boot from the device the node is currently
	// running from, unless safety.allowUnsafeBootOrder is set.
	// +kubebuilder:validation:Pattern=`^0[xX][0-9a-fA-F]{1,8}$`
	// +optional
	BootOrder string `json:"bootOrder,omitempty"`

	// Settings are additional bootloader keys, e.g. POWER_OFF_ON_HALT: "1".
	// BOOT_ORDER must be set with bootOrder, not here.
	// +optional
	Settings map[string]string `json:"settings,omitempty"`
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
