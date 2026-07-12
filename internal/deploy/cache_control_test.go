package tofu

import "testing"

// TestCacheControlForKey pins the Cache-Control the CDN sync writes onto S3
// objects: content-stable media is immutable for a year, while assets that can
// change under the same key across deploys (styles.css, per-page .wasm[.br|.gz])
// get a bounded TTL so a redeploy is never masked by an immutable browser cache.
func TestCacheControlForKey(t *testing.T) {
	const immutable = "public, max-age=31536000, immutable"
	const bounded = "public, max-age=86400, stale-while-revalidate=604800"
	cases := map[string]string{
		"public/logo/original.webp":   immutable,
		"public/logo/blurred.png":     immutable,
		"public/photo/original.jpg":   immutable,
		"public/hero/original.jpeg":   immutable,
		"public/icon.svg":             immutable,
		"public/favicon.ico":          immutable,
		"public/styles.css":           bounded,
		"public/wasm/counter.wasm":    bounded,
		"public/wasm/counter.wasm.br": bounded,
		"public/wasm/counter.wasm.gz": bounded,
		"public/data.json":            bounded,
		"public/wasm_exec_go.js":      bounded,
	}
	for key, want := range cases {
		if got := cacheControlForKey(key); got != want {
			t.Errorf("cacheControlForKey(%q) = %q, want %q", key, got, want)
		}
	}
}
