package executor

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDecodedCodexResponseOwnsTransportMetadata(t *testing.T) {
	body := io.NopCloser(strings.NewReader("fixture"))
	original := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Length": []string{"7"},
			"X-Fixture":      []string{"original"},
		},
		Body: body, ContentLength: 7,
	}
	decoded, err := decodeCodexUpstreamResponse(original)
	if err != nil {
		t.Fatal(err)
	}
	if decoded == original {
		t.Fatal("decoder retained the transport-owned response")
	}
	decoded.Header.Set("X-Fixture", "changed")
	if original.Body != body || original.ContentLength != 7 ||
		original.Header.Get("Content-Length") != "7" ||
		original.Header.Get("X-Fixture") != "original" {
		t.Fatal("decoder mutated transport-owned response fields")
	}
	if decoded.ContentLength != -1 || decoded.Header.Get("Content-Length") != "" {
		t.Fatal("decoded response kept its encoded content length")
	}
	if errClose := decoded.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
}
