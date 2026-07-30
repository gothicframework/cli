package cmd

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// defaultListenAddr is the default HTTP listen address for the app server when
// HTTP_LISTEN_ADDR is not set. It matches the framework's dev-server default.
const defaultListenAddr = ":60714"

// resolveListenAddr returns the listen address configured for the app server
// from the HTTP_LISTEN_ADDR environment variable, falling back to the default
// when the variable is empty or unset.
func resolveListenAddr() string {
	addr := os.Getenv("HTTP_LISTEN_ADDR")
	if addr == "" {
		return defaultListenAddr
	}
	return addr
}

// dialAddr converts a listen address into one suitable for net.Dial from the
// local machine. Bare ports (":60714") get a "localhost" prefix; "0.0.0.0"
// is replaced with "localhost" since we are connecting from the same machine.
// Already-qualified addresses like "localhost:9999" or "127.0.0.1:8080" pass
// through unchanged.
func dialAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not a valid host:port pair, let the caller's Dial fail naturally.
		return addr
	}
	if host == "" || host == "0.0.0.0" {
		return "localhost:" + port
	}
	return addr
}

// listenAddrToURL converts a listen address into a *url.URL suitable for use
// as the reverse-proxy target. It replaces wildcard hosts ("0.0.0.0", "")
// with "localhost" so the proxy connects to the loopback interface.
func listenAddrToURL(addr string) (*url.URL, error) {
	if strings.HasPrefix(addr, ":") {
		return url.Parse("http://localhost" + addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if host == "" || host == "0.0.0.0" {
		return url.Parse("http://localhost:" + port)
	}
	return url.Parse("http://" + addr)
}

// waitForPort repeatedly dials addr (tcp) until the connection succeeds or the
// budget expires. Each attempt has a 100ms timeout; the interval between
// attempts is 50ms. Returns true when the port accepts a connection, false
// when the budget runs out or ctx is cancelled. Expiration is not an error —
// the caller's transport retries on its own.
func waitForPort(ctx context.Context, addr string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
	dialAddr := dialAddr(addr)
	for time.Now().Before(deadline) {
		conn, err := dialer.DialContext(ctx, "tcp", dialAddr)
		if err == nil {
			conn.Close()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return false
}
