package eeprom

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Sig renders the pieeprom.sig the bootloader expects next to pieeprom.upd,
// matching rpi-eeprom-digest: the image's SHA-256, an update timestamp and the
// target SoC. soc is the numeric part of the chip name, e.g. "2712".
func Sig(image []byte, now time.Time, soc string) []byte {
	sum := sha256.Sum256(image)
	return []byte(fmt.Sprintf("%s\nts: %d\ntarget-soc: %s\n", hex.EncodeToString(sum[:]), now.UTC().Unix(), soc))
}
