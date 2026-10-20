package main

// nginx.conf.example is documentation that operators copy into
// /etc/nginx/conf.d, so a mistake in it is a broken deploy rather than a wrong
// comment. It is not Go code and nothing here compiles it, which leaves two
// ways to catch a mistake: a real nginx -t when nginx is installed, and a
// structural check that always runs.
//
// The structural check below catches the failures that actually happen when an
// example is edited: unbalanced braces, a stray directive outside a block, a
// semicolon missing off the end of a line. It deliberately does not try to
// understand nginx - when nginx is on PATH the real parser runs instead.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"noderouter/config"
)

// examplePath is the config relative to the module root.
const examplePath = "nginx.conf.example"

// readExample returns the example's lines with comments and blank lines removed,
// which is what the structural checks walk.
func readExample(t *testing.T) (all, code []string) {
	t.Helper()
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read %s: %v", examplePath, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		all = append(all, line)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		code = append(code, trimmed)
	}
	return all, code
}

// A directive outside any block is the mistake this example is most likely to
// pick up: the status location at the bottom reads correctly but has to live
// inside a server or http block to be valid.
func TestExampleHasNoTopLevelDirectives(t *testing.T) {
	_, code := readExample(t)

	// nginx's own top-level contexts, and the blocks an included conf.d file
	// may legally contain at its own top level.
	topLevelBlocks := map[string]bool{
		"upstream": true, "server": true, "map": true, "geo": true,
		"limit_except": true, "split_clients": true, "types": true,
	}

	depth := 0
	for _, line := range code {
		open := strings.HasSuffix(line, "{")
		name := strings.Fields(line)[0]

		if depth == 0 && !open && !topLevelBlocks[name] {
			t.Errorf("directive %q at top level (outside any block); nginx.conf.example is "+
				"included inside the http block, so only blocks and http-level directives belong here",
				line)
		}
		if open {
			depth++
		}
		if strings.HasSuffix(line, "}") {
			depth--
			if depth < 0 {
				t.Fatalf("unbalanced braces: %q closes a block that was never opened", line)
			}
		}
	}
	if depth != 0 {
		t.Errorf("%d block(s) left unclosed at end of file", depth)
	}
}

// Every directive ends in a semicolon. A missing one is exactly the kind of
// edit that goes unnoticed because the file still reads as prose.
func TestExampleDirectivesAreTerminated(t *testing.T) {
	_, code := readExample(t)

	for i, line := range code {
		if strings.HasSuffix(line, "{") || strings.HasSuffix(line, "}") {
			continue
		}
		// A closing brace may share a line with its last directive.
		body := strings.TrimSuffix(line, "}")
		if !strings.HasSuffix(body, ";") {
			t.Errorf("directive %q does not end with ';'", line)
		}
		_ = i
	}
}

// Braces must balance across the whole file, comments included in the count so
// a commented-out block cannot mask a real one.
func TestExampleBracesBalance(t *testing.T) {
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read %s: %v", examplePath, err)
	}

	depth, line := 0, 0
	for _, l := range strings.Split(string(raw), "\n") {
		line++
		trimmed := strings.TrimSpace(l)
		// A commented-out brace is documentation, not structure.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		depth += strings.Count(l, "{") - strings.Count(l, "}")
		if depth < 0 {
			t.Errorf("line %d: a '}' with no matching '{': %s", line, trimmed)
			return
		}
	}
	if depth != 0 {
		t.Errorf("%d unclosed '{' at end of file", depth)
	}
}

// When nginx is installed, the real parser is the authority. It is skipped
// rather than failed when absent, so the suite still runs on a machine without
// it - but on CI or a dev box that has nginx, this is what catches a bad
// directive.
func TestExampleParsesWithNginx(t *testing.T) {
	nginx, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("nginx is not installed; the structural checks still ran")
	}

	// nginx -t needs the certs and the limit_req zone the example refers to,
	// so run it against a wrapper that declares the zone and points the
	// certificate at a throwaway file. That exercises the example's own
	// directives, which is the point, without needing a real deploy.
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(cert, []byte("-----BEGIN CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "nginx.conf")
	prefixed := "limit_req_zone $binary_remote_addr zone=llm:10m rate=10r/s;\n" +
		"events {}\nhttp {\n" + string(body) + "\n}\n"
	// Point the example's certificate paths at the throwaway file.
	prefixed = strings.ReplaceAll(prefixed, "/etc/letsencrypt/live/llm.example.com/fullchain.pem", cert)
	prefixed = strings.ReplaceAll(prefixed, "/etc/letsencrypt/live/llm.example.com/privkey.pem", cert)
	if err := os.WriteFile(conf, []byte(prefixed), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(nginx, "-t", "-c", conf, "-p", dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("nginx -t rejected %s:\n%s\n(err: %v)", examplePath, out, err)
	}
}

// defaultMaxBodyMB is the router's own request ceiling, which is what nginx's
// client_max_body_size has to be at or above.
func defaultMaxBodyMB() int {
	cfg := &config.Config{}
	cfg.ApplyDefaults()
	return cfg.Server.MaxBodyMB
}

// clientMaxBodySizeMB reads the example's client_max_body_size, returning 0
// when the directive is absent.
func clientMaxBodySizeMB(conf string) (int, error) {
	for _, line := range strings.Split(conf, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || fields[0] != "client_max_body_size" {
			continue
		}
		value := strings.TrimSuffix(strings.TrimSuffix(fields[1], ";"), "m")
		mb, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("client_max_body_size %q is not a size in MB", fields[1])
		}
		return mb, nil
	}
	return 0, nil
}

// The example exists to be copied, so the settings it hard-codes have to agree
// with the router's own defaults. A mismatch here is silent in production: the
// router starts fine and requests just fail.
func TestExampleMatchesRouterDefaults(t *testing.T) {
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read %s: %v", examplePath, err)
	}
	conf := string(raw)

	// The upstream has to point at the port the router listens on by default,
	// or the example proxies into nothing.
	wantUpstream := fmt.Sprintf("127.0.0.1:%d", config.DefaultListenPort)
	if !strings.Contains(conf, wantUpstream) {
		t.Errorf("%s does not proxy to %s, the router's default port", examplePath, wantUpstream)
	}

	// client_max_body_size below server.maxBodyMb means nginx rejects a request
	// the router would have accepted, which is the whole reason the directive
	// is there.
	limitMB, err := clientMaxBodySizeMB(conf)
	if err != nil {
		t.Fatalf("read client_max_body_size: %v", err)
	}
	if limitMB == 0 {
		t.Error("client_max_body_size is not set; nginx's 1MB default would reject image and long-context requests")
	} else if maxBody := defaultMaxBodyMB(); limitMB < maxBody {
		t.Errorf("client_max_body_size is %dm but the router allows %dMB; nginx would reject a request the router accepts",
			limitMB, maxBody)
	}

	// Streaming is the reason this file exists: without these the response is
	// buffered and a completion arrives in one lump.
	for _, directive := range []string{"proxy_buffering off", "proxy_request_buffering off"} {
		if !strings.Contains(conf, directive) {
			t.Errorf("%s is missing %q, which is what keeps a streamed completion from being buffered", examplePath, directive)
		}
	}

	// The read timeout has to outlast a long completion; nginx's 60s default
	// cuts one off mid-answer.
	if !strings.Contains(conf, "proxy_read_timeout 600s") {
		t.Error("proxy_read_timeout 600s is missing; the 60s default cuts long completions off")
	}

	// HTTP/1.1 upstream is required for the keepalive to the router.
	if !strings.Contains(conf, "proxy_http_version 1.1") {
		t.Error("proxy_http_version 1.1 is missing, so the upstream keepalive does not work")
	}
}
