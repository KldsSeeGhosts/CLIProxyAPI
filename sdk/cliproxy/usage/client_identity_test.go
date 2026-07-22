package usage

import (
	"context"
	"strings"
	"testing"
)

func TestClientIdentitySanitizesAndBoundsValues(t *testing.T) {
	ctx := WithClientIdentity(context.Background(), "  Codex\nDesktop\x00  ", strings.Repeat("ua", 400))
	identity := ClientIdentityFromContext(ctx)
	if identity.Originator != "Codex Desktop" {
		t.Fatalf("originator = %q, want %q", identity.Originator, "Codex Desktop")
	}
	if len(identity.UserAgent) != maxClientUserAgentBytes {
		t.Fatalf("user agent bytes = %d, want %d", len(identity.UserAgent), maxClientUserAgentBytes)
	}
	if strings.ContainsAny(identity.Originator, "\r\n\x00") {
		t.Fatalf("originator contains control characters: %q", identity.Originator)
	}
}

func TestClientIdentityEmptyAndNonHTTPContextsRemainCompatible(t *testing.T) {
	ctx := context.Background()
	if got := WithClientIdentity(ctx, "", ""); got != ctx {
		t.Fatal("empty client identity should preserve the original context")
	}
	if got := ClientIdentityFromContext(nil); got != (ClientIdentity{}) {
		t.Fatalf("nil context identity = %#v", got)
	}
}
