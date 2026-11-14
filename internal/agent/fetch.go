package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxImageBytes bounds a download; EEPROM images are at most 2 MiB.
const maxImageBytes = 2*1024*1024 + 1

// Fetcher downloads an EEPROM image and verifies its digest.
type Fetcher interface {
	Fetch(ctx context.Context, url, sha256hex string) ([]byte, error)
}

// HTTPFetcher fetches over HTTPS.
type HTTPFetcher struct {
	Client *http.Client
}

// Fetch implements Fetcher. The digest is checked before the bytes are
// returned, so a tampered or truncated download never reaches the parser.
func (f HTTPFetcher) Fetch(ctx context.Context, url, want string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("refusing non-https image URL %q", url)
	}
	c := f.Client
	if c == nil {
		c = &http.Client{Timeout: 2 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes))
	if err != nil {
		return nil, err
	}
	if len(b) >= maxImageBytes {
		return nil, fmt.Errorf("image at %s is larger than %d bytes", url, maxImageBytes-1)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("image at %s has sha256 %s, want %s", url, got, want)
	}
	return b, nil
}
