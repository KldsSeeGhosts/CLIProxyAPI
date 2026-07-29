package logging

import (
	"strings"
	"testing"
)

func TestSanitizeClientIdentityBoundsAndRemovesControls(t *testing.T) {
	originator, userAgent := SanitizeClientIdentity("  Codex\nDesktop\x00  ", strings.Repeat("ua", 400))
	if originator != "Codex Desktop" {
		t.Fatalf("originator = %q", originator)
	}
	if len(userAgent) != maxClientUserAgentBytes {
		t.Fatalf("user agent bytes = %d, want %d", len(userAgent), maxClientUserAgentBytes)
	}
}
