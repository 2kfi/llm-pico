// version package tests live against the fallback path: in tests the binary
// is built from source, so only the build-info and "dev" branches can run.
package version

import (
	"strings"
	"testing"
)

// String must always return something usable: an explicit build-time stamp,
// a short VCS revision for source builds, or plain "dev".
func TestStringNeverEmpty(t *testing.T) {
	v := String()
	if v == "" {
		t.Fatal("version string is empty")
	}
	for _, bad := range []string{"(", "devel-\x00"} {
		if strings.Contains(v, bad) {
			t.Errorf("version %q contains unexpected %q", v, bad)
		}
	}
}

// Table driving the sub-branches directly would need stubbing debug
// info; an acceptable assertion for a source build is that String
// neither panics nor returns "(devel)" raw, and one explicit build-time
// value flows through unchanged.
func TestBuildTimeValueWins(t *testing.T) {
	pkgVersion = "v1.2.3-test"
	if String() != "v1.2.3-test" {
		t.Fatalf("String() = %q, want the explicit build-time value", String())
	}
	pkgVersion = ""
}
