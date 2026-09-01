package helpers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newChecksumServer returns an httptest.Server that serves the given body
// as the checksums.txt response.
func newChecksumServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
}

func TestFetchExpectedChecksum_UnixLineEndings(t *testing.T) {
	body := "abc123  tinygo-linux-amd64-v0.30.0.tar.gz\ndef456  tinygo-darwin-amd64-v0.30.0.tar.gz\n"
	srv := newChecksumServer(t, body)
	defer srv.Close()

	h := &WasmHelper{}
	got, err := h.fetchExpectedChecksum(srv.URL, "tinygo-darwin-amd64-v0.30.0.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "def456" {
		t.Errorf("got %q, want %q", got, "def456")
	}
}

func TestFetchExpectedChecksum_WindowsLineEndings(t *testing.T) {
	body := "abc123  tinygo-linux-amd64-v0.30.0.tar.gz\r\ndef456  tinygo-darwin-amd64-v0.30.0.tar.gz\r\n"
	srv := newChecksumServer(t, body)
	defer srv.Close()

	h := &WasmHelper{}
	got, err := h.fetchExpectedChecksum(srv.URL, "tinygo-darwin-amd64-v0.30.0.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// bufio.Scanner trims \r\n along with \n via strings.TrimSpace below,
	// so the matched checksum must be exactly "def456".
	if got != "def456" {
		t.Errorf("got %q, want %q", got, "def456")
	}
}

func TestFetchExpectedChecksum_Empty(t *testing.T) {
	srv := newChecksumServer(t, "")
	defer srv.Close()

	h := &WasmHelper{}
	_, err := h.fetchExpectedChecksum(srv.URL, "anything.tar.gz")
	if err == nil {
		t.Fatal("expected error for empty body, got nil")
	}
	if !strings.Contains(err.Error(), "checksum not found") {
		t.Errorf("error mismatch: got %v", err)
	}
}

func TestFetchExpectedChecksum_SkipsCommentsAndBlank(t *testing.T) {
	body := "# header comment\n\nabc123  tinygo-linux-amd64-v0.30.0.tar.gz\n"
	srv := newChecksumServer(t, body)
	defer srv.Close()

	h := &WasmHelper{}
	got, err := h.fetchExpectedChecksum(srv.URL, "tinygo-linux-amd64-v0.30.0.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abc123" {
		t.Errorf("got %q, want %q", got, "abc123")
	}
}

func TestFetchExpectedChecksum_StripsAsteriskPrefix(t *testing.T) {
	body := "abc123 *tinygo-linux-amd64-v0.30.0.tar.gz\n"
	srv := newChecksumServer(t, body)
	defer srv.Close()

	h := &WasmHelper{}
	got, err := h.fetchExpectedChecksum(srv.URL, "tinygo-linux-amd64-v0.30.0.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abc123" {
		t.Errorf("got %q, want %q", got, "abc123")
	}
}

func TestFetchReleaseAssetDigest(t *testing.T) {
	const digest = "b87688fa2e19cee7d813cad7fd7dadb71dff3198e47125aba66ba4af5e490438"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want GitHub JSON media type", got)
		}
		fmt.Fprintf(w, `{"assets":[{"name":"tinygo0.42.0.linux-amd64.tar.gz","digest":"sha256:%s"}]}`, digest)
	}))
	defer srv.Close()

	h := &WasmHelper{}
	got, err := h.fetchReleaseAssetDigest(srv.URL, "tinygo0.42.0.linux-amd64.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != digest {
		t.Errorf("got %q, want %q", got, digest)
	}
}

func TestFetchReleaseAssetDigestRejectsMissingDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"assets":[{"name":"tinygo0.42.0.linux-amd64.tar.gz","digest":null}]}`)
	}))
	defer srv.Close()

	h := &WasmHelper{}
	if _, err := h.fetchReleaseAssetDigest(srv.URL, "tinygo0.42.0.linux-amd64.tar.gz"); err == nil {
		t.Fatal("expected missing digest to fail closed")
	}
}
