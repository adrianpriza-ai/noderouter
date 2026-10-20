package proxy

// A proxy URL may carry the gateway's own credentials (user:pass@host), and
// every outbound spelling of it - response header, 502 body, log line - has to
// name the hop without handing out that password.

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"noderouter/logx"
)

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://gwuser:s3cr3t@127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"socks5://user:pass@10.0.0.1:1080", "socks5://10.0.0.1:1080"},
		{"http://user@host:1", "http://host:1"},
		// a password may itself contain @; the authority ends at the last one
		{"http://gwuser:p@ss@host:1", "http://host:1"},
		// nothing to strip: unchanged, byte for byte
		{"http://1.2.3.4:8080", "http://1.2.3.4:8080"},
		{"socks5://9.9.9.9:2", "socks5://9.9.9.9:2"},
		{"", ""},
		{"1.2.3.4:8080", "1.2.3.4:8080"},
		// an @ in the path or query is content, not credentials
		{"http://host/a@b", "http://host/a@b"},
		{"http://host?a=b@c", "http://host?a=b@c"},
	}
	for _, c := range cases {
		if got := RedactURL(c.in); got != c.want {
			t.Errorf("RedactURL(%q) = %q, want %q", c.in, got, c.want)
		}
		if c.in != c.want && strings.Contains(RedactURL(c.in), "s3cr3t") {
			t.Errorf("RedactURL(%q) still carries credentials: %q", c.in, RedactURL(c.in))
		}
	}
}

// ParseProxyURL errors travel to the client inside the 502 body, so neither
// the parse failure nor the no-host case may echo the credentials back.
func TestParseProxyURLNeverEchoesCredentials(t *testing.T) {
	for _, raw := range []string{
		"http://gwuser:s3cr3t@",       // parses, but has no host
		"http://gwuser:s3cr3t@[::1",   // parse failure: unclosed IPv6
		"http://gwuser:s3cr3t@ho\tst", // parse failure: control character
	} {
		_, err := ParseProxyURL(raw)
		if err == nil {
			t.Errorf("ParseProxyURL(%q): want an error", raw)
			continue
		}
		if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "gwuser") {
			t.Errorf("ParseProxyURL(%q) error leaks credentials: %v", raw, err)
		}
	}
}

// The bench log is the fourth place the URL is written out. Swap stderr for a
// pipe the way logx's own tests do, fail a credentialed proxy into cooldown,
// and read back what was logged.
func TestBenchedLogRedactsProxyCredentials(t *testing.T) {
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	origStd := logx.Std
	logx.Std = logx.New(false)
	defer func() {
		os.Stderr = origStderr
		logx.Std = origStd
	}()

	p := newLoadedPool(t, "rotate", "http://1.1.1.1:8080")
	entry := ProxyEntry{URL: "http://gwuser:s3cr3t@1.1.1.1:8080"}
	for i := 0; i < MaxProxyFails; i++ {
		p.Failed(entry, errors.New("proxyconnect tcp: dial tcp 1.1.1.1:8080: connect: connection refused"))
	}
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(out)
	if !strings.Contains(logged, "proxy benched") {
		t.Fatalf("bench warning was not logged: %q", logged)
	}
	if strings.Contains(logged, "s3cr3t") || strings.Contains(logged, "gwuser") {
		t.Errorf("bench log leaks proxy credentials: %q", logged)
	}
	if !strings.Contains(logged, "1.1.1.1:8080") {
		t.Errorf("bench log lost the hop: %q", logged)
	}
}
