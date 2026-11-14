package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Names the bootloader looks for on the boot partition.
const (
	UpdateFile = "pieeprom.upd"
	SigFile    = "pieeprom.sig"
)

// Stage writes an update for the bootloader's self-update to apply on the next
// boot. The signature is the commit marker: any previous pair is removed first,
// the image is written next, and the .sig last, each atomically and fsynced, so
// the bootloader never sees a signature without its image.
func Stage(bootfs string, image, sig []byte) error {
	if err := Unstage(bootfs); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(bootfs, UpdateFile), image); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(bootfs, SigFile), sig)
}

// Unstage removes any staged update.
func Unstage(bootfs string) error {
	// Remove the signature first so a half-removed pair is inert.
	for _, n := range []string{SigFile, UpdateFile} {
		if err := os.Remove(filepath.Join(bootfs, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return syncDir(bootfs)
}

// StagedDigest returns the SHA-256 of the staged image after checking it still
// matches the digest in its signature file. ok is false if nothing is staged.
func StagedDigest(bootfs string) (digest string, ok bool, err error) {
	img, err := os.ReadFile(filepath.Join(bootfs, UpdateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	sig, err := os.ReadFile(filepath.Join(bootfs, SigFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, fmt.Errorf("%s present without %s", UpdateFile, SigFile)
	}
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(img)
	got := hex.EncodeToString(sum[:])
	want, _, _ := strings.Cut(string(bytes.TrimSpace(sig)), "\n")
	if got != strings.TrimSpace(want) {
		return "", false, errors.New("staged image does not match its signature file")
	}
	return got, true, nil
}

func writeAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	// Best effort: some filesystems (FAT on certain kernels) reject fsync on a
	// directory, and the files themselves are already synced.
	_ = d.Sync()
	return nil
}
