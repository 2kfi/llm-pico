// Package version reports the binary's version string.
//
// The release workflow sets pkgVersion through -ldflags from the git tag, so
// a published binary always reports the tag it was built from. Plain-source
// builds fall back to the embedded VCS revision, then to "dev".
package version

import (
	"fmt"
	"runtime/debug"
)

// pkgVersion is overwritten at build time via
// -ldflags="-X github.com/arkan/llm-pico/internal/version.pkgVersion=vX.Y.Z".
var pkgVersion string

// String returns the version explicitly set at build time, else the
// module version or short VCS revision reported by the runtime build info,
// else "dev".
func String() string {
	if pkgVersion != "" {
		return pkgVersion
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				for _, d := range bi.Settings {
					if d.Key == "vcs.modified" && d.Value == "true" {
						return fmt.Sprintf("devel-%s-dirty", s.Value[:7])
					}
				}
				return "devel-" + s.Value[:7]
			}
		}
	}
	return "dev"
}
