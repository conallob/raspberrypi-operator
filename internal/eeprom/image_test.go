package eeprom

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// buildImage assembles a synthetic 512 KiB image with a read-only bootcode
// section and a writable bootconf.txt section, in the same shape as a real
// pieeprom image: [bootcode][pad][bootconf.txt][erased space][scratch page].
func buildImage(t testing.TB, conf string, size int) []byte {
	t.Helper()
	b := bytes.Repeat([]byte{0xff}, size)
	// bootcode: magic section at 0 with 0x1000 bytes of payload.
	binary.BigEndian.PutUint32(b[0:], magic)
	binary.BigEndian.PutUint32(b[4:], 0x1000)
	for i := 0; i < 0x1000; i++ {
		b[8+i] = byte(i)
	}
	// padding section up to 0x20000 (inside the writable region).
	padOff := (8 + 0x1000 + 7) &^ 7
	binary.BigEndian.PutUint32(b[padOff:], padMagic)
	binary.BigEndian.PutUint32(b[padOff+4:], uint32(0x20000-padOff-8))
	// bootconf.txt at 0x20000.
	off := 0x20000
	binary.BigEndian.PutUint32(b[off:], fileMagic)
	binary.BigEndian.PutUint32(b[off+4:], uint32(len(conf)+sectionPad))
	copy(b[off+8:off+headerLen], append([]byte(BootConfName), make([]byte, filenameSz)...)[:filenameSz])
	binary.BigEndian.PutUint32(b[off+headerLen:], 0) // reserved word
	copy(b[off+dataOffset:], conf)
	return b
}

func TestImageRoundTrip(t *testing.T) {
	for _, size := range []int{512 * 1024, 2 * 1024 * 1024} {
		orig := "[all]\nBOOT_UART=1\nBOOT_ORDER=0xf41\n"
		img, err := ParseImage(buildImage(t, orig, size))
		if err != nil {
			t.Fatal(err)
		}
		got, err := img.Config()
		if err != nil || got != orig {
			t.Fatalf("Config() = %q, %v; want %q", got, err, orig)
		}

		// Grow the config, shrink it, and leave the bootcode alone.
		before := img.Bytes()
		for _, next := range []string{
			orig + strings.Repeat("# comment line to grow the file\n", 40),
			"BOOT_ORDER=0xf416\n",
			orig,
		} {
			if err := img.SetConfig(next); err != nil {
				t.Fatalf("SetConfig: %v", err)
			}
			got, _ := img.Config()
			if got != next {
				t.Fatalf("round trip: got %q want %q", got, next)
			}
			if !bytes.Equal(img.Bytes()[:0x2000], before[:0x2000]) {
				t.Fatal("bootcode region changed")
			}
			// Edited image must still parse (and so can be re-flashed).
			if _, err := ParseImage(img.Bytes()); err != nil {
				t.Fatalf("edited image does not re-parse: %v", err)
			}
		}
	}
}

func TestImageSetConfigTooLarge(t *testing.T) {
	img, _ := ParseImage(buildImage(t, "A=1\n", 512*1024))
	before := img.Bytes()
	if err := img.SetConfig(strings.Repeat("x", MaxConfigSize)); err == nil {
		t.Fatal("expected an error for an oversize config")
	}
	if !bytes.Equal(before, img.Bytes()) {
		t.Fatal("failed SetConfig must not modify the image")
	}
}

func TestParseImageRejects(t *testing.T) {
	if _, err := ParseImage(make([]byte, 1000)); err == nil {
		t.Error("wrong size accepted")
	}
	bad := bytes.Repeat([]byte{0xff}, 512*1024)
	binary.BigEndian.PutUint32(bad[0:], 0xdeadbeef)
	if _, err := ParseImage(bad); err == nil {
		t.Error("bad magic accepted")
	}
	empty := bytes.Repeat([]byte{0xff}, 512*1024)
	if _, err := ParseImage(empty); err == nil {
		t.Error("image with no sections accepted")
	}
}

func TestSig(t *testing.T) {
	got := string(Sig([]byte("abc"), time.Unix(1700000000, 0), "2712"))
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad\nts: 1700000000\ntarget-soc: 2712\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
