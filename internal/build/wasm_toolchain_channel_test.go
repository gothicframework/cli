package helpers

import "testing"

// TestTinyGoReleaseBaseURL_RoutesPatchedToFork verifies the patched-TinyGo
// channel routing: a <semver>-gothic.<n> pin resolves to the maintainer's fork
// releases, while an official version resolves to upstream. The match is by
// convention (regex), never hardcoded to a specific patch tag.
func TestTinyGoReleaseBaseURL_RoutesPatchedToFork(t *testing.T) {
	cases := []struct {
		version string
		want    string
	}{
		// Official pins → upstream.
		{"0.41.1", tinyGoUpstreamReleases},
		{"0.42.0", tinyGoUpstreamReleases},
		{"1.0.0", tinyGoUpstreamReleases},
		// Patched pins → fork.
		{"0.41.1-gothic.1", tinyGoForkReleases},
		{"0.42.0-gothic.3", tinyGoForkReleases},
		{"0.41.1-gothic.12", tinyGoForkReleases},
		// Near-misses that must NOT route to the fork.
		{"0.41.1-gothic", tinyGoUpstreamReleases},   // missing .<n>
		{"0.41.1-gothic.1a", tinyGoUpstreamReleases}, // non-numeric suffix
		{"0.41.1-beta.1", tinyGoUpstreamReleases},    // different prerelease
		{"gothic.1", tinyGoUpstreamReleases},         // no base semver
		{"", tinyGoUpstreamReleases},                 // empty
	}
	for _, c := range cases {
		if got := tinyGoReleaseBaseURL(c.version); got != c.want {
			t.Errorf("tinyGoReleaseBaseURL(%q) = %q, want %q", c.version, got, c.want)
		}
	}
}
