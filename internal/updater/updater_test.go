package updater

import (
	"strings"
	"testing"
)

func TestParseAptPackageVersion(t *testing.T) {
	sample := `
Package: other-pkg
Version: 1.0.0

Package: claude-desktop
Architecture: arm64
Version: 5.11492.5

Package: claude-desktop
Architecture: arm64
Version: 5.12000.1
`
	got := parseAptPackageVersion(strings.NewReader(sample), "claude-desktop")
	if got != "5.12000.1" {
		t.Fatalf("expected 5.12000.1, got %q", got)
	}
}
