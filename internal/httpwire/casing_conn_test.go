package httpwire

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

// The ZCode emulation sends every header lowercase except Content-Type; the
// casing conn must produce exactly those bytes on the wire.
func TestCasingConnLowercasesExceptAllowlist(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	wrapped := NewCasingConn(client, func(_, _, name string) string {
		if strings.EqualFold(name, "Content-Type") {
			return "Content-Type"
		}
		return strings.ToLower(name)
	})

	var wg sync.WaitGroup
	var writeErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, writeErr = wrapped.Write([]byte("POST /v1/messages HTTP/1.1\r\n" +
			"Content-Type: application/json\r\n" +
			"X-Zcode-App-Version: 3.10.1\r\n" +
			"User-Agent: ZCode/3.10.1\r\n" +
			"Content-Length: 2\r\n" +
			"\r\n" +
			"{}"))
		_ = client.Close()
	}()

	reader := bufio.NewReader(server)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "POST /v1/messages HTTP/1.1" {
		t.Fatalf("request line altered: %q", line)
	}
	got := map[string]string{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		got[parts[0]] = strings.TrimSpace(parts[1])
	}
	for name, want := range map[string]string{
		"Content-Type":        "application/json",
		"x-zcode-app-version": "3.10.1",
		"user-agent":          "ZCode/3.10.1",
		"content-length":      "2",
	} {
		if got[name] != want {
			t.Errorf("wire %q = %q, want %q", name, got[name], want)
		}
	}
	if _, ok := got["X-Zcode-App-Version"]; ok {
		t.Error("X-Zcode-App-Version not lowercased on the wire")
	}
	body := make([]byte, 2)
	if _, err := io.ReadFull(reader, body); err != nil || string(body) != "{}" {
		t.Fatalf("body passthrough broken: %q err=%v", body, err)
	}

	wg.Wait()
	if writeErr != nil {
		t.Errorf("Write returned error: %v", writeErr)
	}
}
