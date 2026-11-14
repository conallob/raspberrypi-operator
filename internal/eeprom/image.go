// Package eeprom reads and edits Raspberry Pi bootloader EEPROM images in
// pure Go. It is a port of the image handling in rpi-eeprom-config from
// https://github.com/raspberrypi/rpi-eeprom, limited to replacing the
// embedded bootconf.txt.
package eeprom

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Image layout constants, from rpi-eeprom-config.
const (
	magic      = 0x55aaf00f
	padMagic   = 0x55aafeef
	magicMask  = 0xfffff00f
	fileMagic  = 0x55aaf11f
	headerLen  = 20 // magic(4) length(4) filename(12)
	filenameSz = 12
	// A modifiable file's data starts 4 bytes after its header; section length
	// covers the filename, that 4 byte word, and the data.
	dataOffset  = headerLen + 4
	sectionPad  = filenameSz + 4
	eraseAlign  = 4096
	maxFileSize = eraseAlign - headerLen

	readOnlySize  = 64 * 1024
	partitionSize = 988 * 1024

	// BootConfName is the name of the configuration file inside an image.
	BootConfName = "bootconf.txt"
	// MaxConfigSize is the largest bootconf.txt an image can hold.
	MaxConfigSize = maxFileSize
)

var validImageSizes = map[int]bool{512 * 1024: true, 2 * 1024 * 1024: true}

type section struct {
	magic    uint32
	offset   int
	length   int
	filename string
}

// Image is a parsed EEPROM image.
type Image struct {
	b        []byte
	sections []section
}

// ParseImage validates and parses an EEPROM image (pieeprom-*.bin). The input
// is copied.
func ParseImage(data []byte) (*Image, error) {
	if !validImageSizes[len(data)] {
		return nil, fmt.Errorf("unexpected EEPROM image size %d (want 524288 or 2097152)", len(data))
	}
	img := &Image{b: append([]byte(nil), data...)}
	if err := img.parse(); err != nil {
		return nil, err
	}
	return img, nil
}

func (m *Image) parse() error {
	m.sections = nil
	off := 0
	for off+8 <= len(m.b) {
		mg := binary.BigEndian.Uint32(m.b[off:])
		length := int(binary.BigEndian.Uint32(m.b[off+4:]))
		if mg == 0 || mg == 0xffffffff {
			break // end of image
		}
		if mg&magicMask != magic {
			return fmt.Errorf("EEPROM image is corrupt: bad section magic %#x at offset %d", mg, off)
		}
		if off+8+length > len(m.b) {
			return fmt.Errorf("EEPROM image is corrupt: section at %d overruns the image", off)
		}
		s := section{magic: mg, offset: off, length: length}
		if mg == fileMagic {
			if off+headerLen > len(m.b) {
				return fmt.Errorf("EEPROM image is corrupt: truncated file header at %d", off)
			}
			s.filename = trimNUL(m.b[off+8 : off+headerLen])
		}
		m.sections = append(m.sections, s)
		off += 8 + length
		off = (off + 7) &^ 7
	}
	if len(m.sections) == 0 {
		return errors.New("EEPROM image is corrupt: no sections")
	}
	return nil
}

func trimNUL(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// findFile locates the modifiable file name within [start, end) (end < 0 means
// no limit). It returns the section index, and the offset at which the next
// non-padding section begins (the room available to the file).
func (m *Image) findFile(name string, start, end int) (idx, next int) {
	next = len(m.b) - eraseAlign // never pad into the bootloader scratch page
	if end >= 0 && end < next {
		next = end
	}
	idx = -1
	for i, s := range m.sections {
		if s.offset < start || (end >= 0 && s.offset >= end) {
			continue
		}
		if s.magic == fileMagic && s.filename == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return -1, next
	}
	for j := idx + 1; j < len(m.sections); j++ {
		if m.sections[j].magic == padMagic {
			continue
		}
		next = m.sections[j].offset
		break
	}
	return idx, next
}

// File returns the contents of the first modifiable file called name, or
// false if there is none.
func (m *Image) File(name string) ([]byte, bool) {
	idx, _ := m.findFile(name, 0, -1)
	if idx < 0 {
		return nil, false
	}
	s := m.sections[idx]
	n := s.length - sectionPad
	if n < 0 || s.offset+dataOffset+n > len(m.b) {
		return nil, false
	}
	return append([]byte(nil), m.b[s.offset+dataOffset:s.offset+dataOffset+n]...), true
}

// Config returns the bootloader configuration embedded in the image.
func (m *Image) Config() (string, error) {
	b, ok := m.File(BootConfName)
	if !ok {
		return "", fmt.Errorf("image has no %s", BootConfName)
	}
	return string(b), nil
}

// SetConfig replaces the embedded bootloader configuration, mirroring
// rpi-eeprom-config's placement and padding rules. The receiver is only
// modified if the whole update succeeds.
func (m *Image) SetConfig(conf string) error {
	src := []byte(conf)
	if len(src) > maxFileSize-4 {
		return fmt.Errorf("bootloader config is %d bytes; the maximum is %d", len(src), maxFileSize-4)
	}
	idx, next := m.findFile(BootConfName, readOnlySize, readOnlySize+partitionSize)
	if idx < 0 {
		return fmt.Errorf("image has no writable %s", BootConfName)
	}
	s := m.sections[idx]
	end := s.offset + dataOffset + len(src)
	if end > len(m.b)-eraseAlign {
		return fmt.Errorf("no space for %s in image", BootConfName)
	}
	if end > next {
		return fmt.Errorf("%s (%d bytes) does not fit in its section (%d bytes available)",
			BootConfName, len(src), next-s.offset-dataOffset)
	}

	out := append([]byte(nil), m.b...)
	binary.BigEndian.PutUint32(out[s.offset+4:], uint32(len(src)+sectionPad))
	copy(out[s.offset+dataOffset:], src)

	padStart := end
	for padStart%8 != 0 {
		out[padStart] = 0xff
		padStart++
	}
	isLast := idx == len(m.sections)-1
	padBytes := next - padStart
	if padBytes >= 8 && !isLast {
		padBytes -= 8
		binary.BigEndian.PutUint32(out[padStart:], padMagic)
		binary.BigEndian.PutUint32(out[padStart+4:], uint32(padBytes))
		padStart += 8
	}
	for i := 0; i < padBytes; i++ {
		out[padStart+i] = 0xff
	}

	re := &Image{b: out}
	if err := re.parse(); err != nil {
		return fmt.Errorf("edited image failed validation: %w", err)
	}
	got, err := re.Config()
	if err != nil || got != conf {
		return errors.New("edited image failed validation: config did not round-trip")
	}
	*m = *re
	return nil
}

// Bytes returns a copy of the image.
func (m *Image) Bytes() []byte { return append([]byte(nil), m.b...) }
