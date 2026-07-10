package tofu

import (
	"encoding/hex"
	"hash/crc32"
)

// ResourceSuffix returns a deterministic 8-character hex suffix derived from the
// Go module name and project name. It replaces the random .gothicCli/app-id.txt
// mechanism used in v2: because it is computed from the (globally unique) module
// path, every developer on the project and every fresh `git clone` produces the
// same suffix, so AWS resource names stay stable without an init-time file.
func ResourceSuffix(goModName, projectName string) string {
	sum := crc32.ChecksumIEEE([]byte(goModName + ":" + projectName))
	b := []byte{
		byte(sum >> 24),
		byte(sum >> 16),
		byte(sum >> 8),
		byte(sum),
	}
	return hex.EncodeToString(b)
}
