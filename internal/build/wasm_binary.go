package helpers

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Binary lifecycle: TinyGo toolchain download, install, verification, and
// per-OS/arch path resolution. Methods here are CLI-side (not compiled into
// WASM).

var ensureBinaryMu sync.Mutex
var ensureBinaryenMu sync.Mutex

const (
	wasmMaxRetries      = 3
	wasmDownloadTimeout = 10 * time.Minute
)

// TinyGo release hosts. Official builds come from the upstream org; Gothic's
// patched builds (a fix that is MERGED upstream but not yet in an official
// release) are hosted on the maintainer's fork under the same release-asset
// naming as upstream. See cli/docs/patched-tinygo-channel.md.
const (
	tinyGoUpstreamReleases = "https://github.com/tinygo-org/tinygo/releases/download"
	tinyGoForkReleases     = "https://github.com/felipegenef/tinygo/releases/download"
)

// gothicPatchedVersion matches Gothic's patched-TinyGo version convention:
// <semver>-gothic.<n> (e.g. "0.41.1-gothic.1"). A version matching this pattern
// is downloaded from the fork; every other version comes from upstream. The
// convention is the ONLY routing signal — nothing is hardcoded to a specific
// patch — so a future 0.42.0-gothic.3 routes to the fork automatically while a
// bare official 0.42.0 stays on upstream.
var gothicPatchedVersion = regexp.MustCompile(`^\d+\.\d+\.\d+-gothic\.\d+$`)

// tinyGoReleaseBaseURL returns the GitHub "releases/download" base the TinyGo
// archive + checksums.txt are fetched from for the given version: the fork for a
// -gothic.<n> patched pin, upstream otherwise.
func tinyGoReleaseBaseURL(version string) string {
	if gothicPatchedVersion.MatchString(version) {
		return tinyGoForkReleases
	}
	return tinyGoUpstreamReleases
}

func (h *WasmHelper) binaryName() (string, error) {
	key := h.Runtime + "/" + h.Arch
	names := map[string]string{
		"linux/amd64":   fmt.Sprintf("tinygo%s.linux-amd64.tar.gz", h.Version),
		"linux/arm64":   fmt.Sprintf("tinygo%s.linux-arm64.tar.gz", h.Version),
		"darwin/amd64":  fmt.Sprintf("tinygo%s.darwin-amd64.tar.gz", h.Version),
		"darwin/arm64":  fmt.Sprintf("tinygo%s.darwin-arm64.tar.gz", h.Version),
		"windows/amd64": fmt.Sprintf("tinygo%s.windows-amd64.zip", h.Version),
	}
	name, ok := names[key]
	if !ok {
		return "", fmt.Errorf("unsupported platform %s/%s for TinyGo", h.Runtime, h.Arch)
	}
	return name, nil
}

func (h *WasmHelper) binaryenBinaryName() (string, error) {
	key := h.Runtime + "/" + h.Arch
	names := map[string]string{
		"linux/amd64":   fmt.Sprintf("binaryen-version_%s-x86_64-linux.tar.gz", h.BinaryenVersion),
		"linux/arm64":   fmt.Sprintf("binaryen-version_%s-aarch64-linux.tar.gz", h.BinaryenVersion),
		"darwin/amd64":  fmt.Sprintf("binaryen-version_%s-x86_64-macos.tar.gz", h.BinaryenVersion),
		"darwin/arm64":  fmt.Sprintf("binaryen-version_%s-arm64-macos.tar.gz", h.BinaryenVersion),
		"windows/amd64": fmt.Sprintf("binaryen-version_%s-x86_64-windows.tar.gz", h.BinaryenVersion),
	}
	name, ok := names[key]
	if !ok {
		return "", fmt.Errorf("unsupported platform %s/%s for Binaryen", h.Runtime, h.Arch)
	}
	return name, nil
}

func (h *WasmHelper) cacheDir() (string, error) {
	base := os.Getenv("GOTHIC_CLI_CACHE_DIR")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("failed to determine user cache directory: %w", err)
		}
	}
	return filepath.Join(base, "gothic-cli", "tinygo"), nil
}

func (h *WasmHelper) BinaryenRoot() string {
	dir, err := h.cacheDir()
	if err != nil {
		return ""
	}
	platform := h.Runtime + "-" + h.Arch
	return filepath.Join(dir, "binaryen-"+h.BinaryenVersion, platform, "binaryen-version_"+h.BinaryenVersion)
}

func (h *WasmHelper) BinaryenBinary() string {
	root := h.BinaryenRoot()
	if root == "" {
		return ""
	}
	name := "wasm-opt"
	if h.Runtime == "windows" {
		name += ".exe"
	}
	return filepath.Join(root, "bin", name)
}

func (h *WasmHelper) TinyGoRoot() string {
	dir, err := h.cacheDir()
	if err != nil {
		return ""
	}
	platform := h.Runtime + "-" + h.Arch
	return filepath.Join(dir, "tinygo-"+h.Version, platform, "tinygo")
}

func (h *WasmHelper) TinyGoBinary() string {
	name := "tinygo"
	if h.Runtime == "windows" {
		name += ".exe"
	}
	return filepath.Join(h.TinyGoRoot(), "bin", name)
}

// effectiveTinyGoRoot returns the TINYGOROOT the tinygo invocation should run
// with. With no override configured, that is the managed toolchain's root. When
// ConfigOverride is set, an override binary carries its own runtime source tree,
// so it must be pointed at its OWN root — pinning it to the managed root makes
// its codegen emit runtime calls the managed root doesn't define, and the build
// panics with "unknown runtime call". The value is normally precomputed by
// EnsureBinary (single-threaded, before parallel builds); the lazy fallback
// covers callers that construct a build command without EnsureBinary first.
func (h *WasmHelper) effectiveTinyGoRoot() string {
	if h.ConfigOverride == "" {
		return h.TinyGoRoot()
	}
	if h.overrideRoot != "" {
		return h.overrideRoot
	}
	return h.overrideTinyGoRoot()
}

// overrideTinyGoRoot resolves the root that matches the override tinygo binary.
// It asks the binary itself — `tinygo env TINYGOROOT` reports the baked-in root —
// and falls back to the binary's grandparent directory when that fails (e.g.
// /path/to/tinygo/build/tinygo → /path/to/tinygo).
func (h *WasmHelper) overrideTinyGoRoot() string {
	if out, err := exec.Command(h.ConfigOverride, "env", "TINYGOROOT").Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root
		}
	}
	return filepath.Dir(filepath.Dir(h.ConfigOverride))
}

func (h *WasmHelper) Environ() []string {
	return h.EnvironWithWarn(nil)
}

func (h *WasmHelper) EnvironWithWarn(warnOnce *sync.Once) []string {
	root := h.effectiveTinyGoRoot()
	binDir := filepath.Join(root, "bin")

	binaryenBinDir := filepath.Join(h.BinaryenRoot(), "bin")

	env := []string{
		"TINYGOROOT=" + root,
		"PATH=" + binDir + string(os.PathListSeparator) + binaryenBinDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	// TinyGo 0.41.1 requires wasm-opt for -opt=s/z. If it's missing from both
	// the system PATH and the managed tinygo/bin dir, and we haven't managed
	// to download our own binaryen either, we set WASMOPT=false.
	hasWasmOpt := false
	if _, err := exec.LookPath("wasm-opt"); err == nil {
		hasWasmOpt = true
	} else if _, err := os.Stat(filepath.Join(binDir, "wasm-opt")); err == nil {
		hasWasmOpt = true
	} else if _, err := os.Stat(filepath.Join(binDir, "wasm-opt.exe")); err == nil {
		hasWasmOpt = true
	} else if _, err := os.Stat(h.BinaryenBinary()); err == nil {
		hasWasmOpt = true
	}

	if !hasWasmOpt {
		env = append(env, "WASMOPT=false")
		if warnOnce != nil {
			warnOnce.Do(func() {
				wasmWarnf("wasm-opt not found; skipping optimization (WASMOPT=false). Install Binaryen for smaller binaries.")
			})
		}
	}

	return env
}

func (h *WasmHelper) EnsureBinaryen() error {
	// If wasm-opt is already in PATH, we don't need to download it.
	if _, err := exec.LookPath("wasm-opt"); err == nil {
		return nil
	}

	if b := h.BinaryenBinary(); b != "" {
		if _, err := os.Stat(b); err == nil {
			return nil
		}
	}

	ensureBinaryenMu.Lock()
	defer ensureBinaryenMu.Unlock()

	if b := h.BinaryenBinary(); b != "" {
		if _, err := os.Stat(b); err == nil {
			return nil
		}
	}

	archiveName, err := h.binaryenBinaryName()
	if err != nil {
		return err
	}

	archiveURL := fmt.Sprintf(
		"https://github.com/WebAssembly/binaryen/releases/download/version_%s/%s",
		h.BinaryenVersion, archiveName,
	)

	fmt.Fprintf(os.Stderr, "wasm: wasm-opt not found — downloading Binaryen %s for %s/%s...\n",
		h.BinaryenVersion, h.Runtime, h.Arch)

	tmpArchive, err := h.downloadToTemp(archiveURL)
	if err != nil {
		return fmt.Errorf("wasm: download Binaryen: %w", err)
	}
	defer os.Remove(tmpArchive)

	dir, err := h.cacheDir()
	if err != nil {
		return err
	}

	platform := h.Runtime + "-" + h.Arch
	finalDir := filepath.Join(dir, "binaryen-"+h.BinaryenVersion, platform)
	tmpDir := finalDir + ".tmp"

	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("wasm: mkdir: %w", err)
	}

	fmt.Fprintln(os.Stderr, "wasm: extracting Binaryen toolchain...")
	if err := h.extractArchive(tmpArchive, tmpDir); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("wasm: extract: %w", err)
	}

	os.RemoveAll(finalDir)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("wasm: install: %w", err)
	}

	fmt.Fprintf(os.Stderr, "wasm: Binaryen %s ready at %s\n", h.BinaryenVersion, h.BinaryenRoot())
	return nil
}

func (h *WasmHelper) EnsureBinary() error {
	if h.ConfigOverride != "" {
		if _, err := os.Stat(h.ConfigOverride); err != nil {
			return fmt.Errorf("wasm binary override not found at %q: %w", h.ConfigOverride, err)
		}
		// Resolve the override binary's own root once, here on the single build
		// thread, so the parallel EnvironWithWarn callers only read it.
		h.overrideRoot = h.overrideTinyGoRoot()
		return nil
	}

	tinygoReady := func() bool {
		_, err := os.Stat(h.TinyGoBinary())
		return err == nil
	}
	binaryenReady := func() bool {
		if _, err := exec.LookPath("wasm-opt"); err == nil {
			return true
		}
		if b := h.BinaryenBinary(); b != "" {
			if _, err := os.Stat(b); err == nil {
				return true
			}
		}
		return false
	}

	if tinygoReady() && binaryenReady() {
		return nil
	}

	var g errgroup.Group
	if !binaryenReady() {
		g.Go(func() error {
			if err := h.EnsureBinaryen(); err != nil {
				fmt.Fprintf(os.Stderr, "wasm: WARNING — failed to ensure Binaryen (%v); build might be unoptimized\n", err)
			}
			return nil
		})
	}
	if !tinygoReady() {
		g.Go(h.ensureTinyGo)
	}
	return g.Wait()
}

func (h *WasmHelper) ensureTinyGo() error {
	ensureBinaryMu.Lock()
	defer ensureBinaryMu.Unlock()

	if _, err := os.Stat(h.TinyGoBinary()); err == nil {
		return nil
	}

	archiveName, err := h.binaryName()
	if err != nil {
		return err
	}

	dir, err := h.cacheDir()
	if err != nil {
		return err
	}

	base := tinyGoReleaseBaseURL(h.Version)
	archiveURL := fmt.Sprintf("%s/v%s/%s", base, h.Version, archiveName)
	checksumURL := fmt.Sprintf("%s/v%s/checksums.txt", base, h.Version)

	fmt.Fprintf(os.Stderr, "wasm: TinyGo %s not found — downloading for %s/%s...\n",
		h.Version, h.Runtime, h.Arch)

	expected, checksumErr := h.fetchExpectedChecksum(checksumURL, archiveName)
	if checksumErr != nil {
		fmt.Fprintf(os.Stderr, "wasm: WARNING — checksums.txt unavailable (%v); proceeding without pre-verification\n", checksumErr)
	}

	tmpArchive, err := h.downloadToTemp(archiveURL)
	if err != nil {
		return fmt.Errorf("wasm: download TinyGo: %w", err)
	}
	defer os.Remove(tmpArchive)

	if expected != "" {
		if err := h.verifyChecksum(tmpArchive, expected); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "wasm: checksum OK")
	} else {
		if digest, err := h.computeChecksum(tmpArchive); err == nil {
			platform := h.Runtime + "-" + h.Arch
			localChecksum := filepath.Join(dir, "tinygo-"+h.Version, platform+".sha256")
			_ = os.MkdirAll(filepath.Dir(localChecksum), 0755)
			_ = os.WriteFile(localChecksum, []byte(digest), 0644)
		}
	}

	platform := h.Runtime + "-" + h.Arch
	finalDir := filepath.Join(dir, "tinygo-"+h.Version, platform)
	tmpDir := finalDir + ".tmp"

	os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("wasm: mkdir: %w", err)
	}

	fmt.Fprintln(os.Stderr, "wasm: extracting TinyGo toolchain...")
	if err := h.extractArchive(tmpArchive, tmpDir); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("wasm: extract: %w", err)
	}

	os.RemoveAll(finalDir)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("wasm: install: %w", err)
	}

	fmt.Fprintf(os.Stderr, "wasm: TinyGo %s ready at %s\n", h.Version, h.TinyGoRoot())
	return nil
}

func (h *WasmHelper) downloadToTemp(url string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= wasmMaxRetries; attempt++ {
		path, err := h.tryDownload(url)
		if err == nil {
			return path, nil
		}
		lastErr = err
		if attempt < wasmMaxRetries {
			delay := 2 * time.Second * time.Duration(attempt)
			fmt.Fprintf(os.Stderr, "wasm: attempt %d/%d failed (%v) — retrying in %s\n",
				attempt, wasmMaxRetries, err, delay)
			time.Sleep(delay)
		}
	}
	return "", fmt.Errorf("download failed after %d attempts: %w", wasmMaxRetries, lastErr)
}

func (h *WasmHelper) tryDownload(url string) (string, error) {
	client := &http.Client{Timeout: wasmDownloadTimeout}
	resp, err := client.Get(url) //nolint:noctx
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	tmp, err := os.CreateTemp("", "tinygo-download-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}

	pr := &wasmProgressReader{r: resp.Body, total: resp.ContentLength}
	if _, err := io.Copy(tmp, pr); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write temp: %w", err)
	}
	fmt.Fprintln(os.Stderr)

	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

type wasmProgressReader struct {
	r     io.Reader
	total int64
	read  int64
}

func (p *wasmProgressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.read += int64(n)
	if p.total > 0 {
		pct := 100 * p.read / p.total
		fmt.Fprintf(os.Stderr, "\rwasm: %d%%  (%d MB / %d MB)", pct, p.read>>20, p.total>>20)
	} else {
		fmt.Fprintf(os.Stderr, "\rwasm: %d MB downloaded", p.read>>20)
	}
	return n, err
}

func (h *WasmHelper) fetchExpectedChecksum(checksumURL, filename string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(checksumURL) //nolint:noctx
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d fetching checksums.txt", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == filename {
			return fields[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("scan checksums: %w", err)
	}
	return "", fmt.Errorf("checksum not found for %q", filename)
}

func (h *WasmHelper) verifyChecksum(filePath, expected string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	hh := sha256.New()
	if _, err := io.Copy(hh, f); err != nil {
		return fmt.Errorf("hash %s: %w", filePath, err)
	}
	actual := hex.EncodeToString(hh.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("SHA-256 mismatch\n  expected: %s\n  actual:   %s", expected, actual)
	}
	return nil
}

func (h *WasmHelper) computeChecksum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hh := sha256.New()
	if _, err := io.Copy(hh, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hh.Sum(nil)), nil
}
