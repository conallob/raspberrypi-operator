package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestHTTPFetcher(t *testing.T) {
	body := bytes.Repeat([]byte("eeprom"), 100)
	big := bytes.Repeat([]byte{1}, maxImageBytes+10)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write(body)
		case "/big":
			_, _ = w.Write(big)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := HTTPFetcher{Client: srv.Client()}
	ctx := context.Background()

	got, err := f.Fetch(ctx, srv.URL+"/ok", digest(body))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("good fetch: %v", err)
	}
	for name, c := range map[string]struct{ path, sha, want string }{
		"digest mismatch": {"/ok", strings.Repeat("0", 64), "has sha256"},
		"404":             {"/missing", digest(body), "404"},
		"oversize":        {"/big", digest(big), "larger than"},
	} {
		if _, err := f.Fetch(ctx, srv.URL+c.path, c.sha); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
	}
	if _, err := f.Fetch(ctx, "http://example.invalid/x", digest(body)); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Errorf("plain http accepted: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Fetch(cctx, srv.URL+"/ok", digest(body)); err == nil {
		t.Error("cancelled context accepted")
	}
}

// A server whose certificate the client does not trust must be refused: the
// default client verifies TLS.
func TestHTTPFetcherVerifiesTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }))
	defer srv.Close()
	if _, err := (HTTPFetcher{}).Fetch(context.Background(), srv.URL, digest([]byte("x"))); err == nil {
		t.Fatal("accepted an untrusted certificate")
	}
}
