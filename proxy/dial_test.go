package proxy

// The SOCKS5 handshake is hand-rolled because the project takes no third-party
// modules, which puts the burden on these tests: a wrong byte here is a
// connection that silently never works, and the paths that are hardest to get
// right are the ones a real proxy rarely exercises (credentials, domains,
// IPv6, rejections).

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// socks5Server is a minimal SOCKS5 proxy for one connection. It records what it
// was asked for so a test can assert on the bytes the client actually sent.
type socks5Server struct {
	ln net.Listener

	// wantAuth is the method the server selects: 0x00 none, 0x02 credentials.
	wantAuth byte
	// user/pass is the credential the server demands when wantAuth is 0x02.
	user, pass string
	// rejectAuth makes the server answer the auth exchange with a failure.
	rejectAuth bool
	// replyCode is the status returned for the CONNECT command.
	replyCode byte
	// bindAddrType is the ATYP the server uses in its reply: 0x01 IPv4,
	// 0x03 domain, 0x04 IPv6, or 0x00 for the minimal IPv4 form.
	bindAddrType byte

	mu       sync.Mutex
	gotAddr  string // "host:port" the client asked to CONNECT to
	gotATYP  byte
	gotUser  string
	gotPass  string
	sawGreet []byte
}

// startSocks5 starts the server on a random port. It shuts down with the test.
func startSocks5(t *testing.T, cfg *socks5Server) *socks5Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := &socks5Server{
		ln:           ln,
		wantAuth:     cfg.wantAuth,
		user:         cfg.user,
		pass:         cfg.pass,
		rejectAuth:   cfg.rejectAuth,
		replyCode:    cfg.replyCode,
		bindAddrType: cfg.bindAddrType,
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *socks5Server) url() *url.URL {
	u, _ := url.Parse("socks5://" + s.ln.Addr().String())
	return u
}

func (s *socks5Server) serve(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)

	// Greeting: ver, nmethods, methods...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	s.mu.Lock()
	s.sawGreet = append([]byte{hdr[0], hdr[1]}, methods...)
	s.mu.Unlock()

	if _, err := conn.Write([]byte{0x05, s.wantAuth}); err != nil {
		return
	}

	// Credentials, when the server asked for them.
	if s.wantAuth == 0x02 {
		vh := make([]byte, 2)
		if _, err := io.ReadFull(br, vh); err != nil || vh[0] != 0x01 {
			return
		}
		u := make([]byte, vh[1])
		if _, err := io.ReadFull(br, u); err != nil {
			return
		}
		pl := make([]byte, 1)
		if _, err := io.ReadFull(br, pl); err != nil {
			return
		}
		p := make([]byte, pl[0])
		if _, err := io.ReadFull(br, p); err != nil {
			return
		}
		s.mu.Lock()
		s.gotUser, s.gotPass = string(u), string(p)
		s.mu.Unlock()

		status := byte(0x00)
		if s.rejectAuth {
			status = 0x01
		}
		if _, err := conn.Write([]byte{0x01, status}); err != nil {
			return
		}
		if status != 0x00 {
			return
		}
	}

	// Request: ver, cmd, rsv, atyp, addr..., port
	reqHead := make([]byte, 4)
	if _, err := io.ReadFull(br, reqHead); err != nil {
		return
	}
	var host string
	switch reqHead[3] {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(br, l); err != nil {
			return
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		host = string(b)
	default:
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(br, pb); err != nil {
		return
	}
	s.mu.Lock()
	s.gotAddr = net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(pb)))
	s.gotATYP = reqHead[3]
	s.mu.Unlock()

	// Reply, including a bound address that has to be drained off the wire.
	reply := []byte{0x05, s.replyCode, 0x00}
	switch s.bindAddrType {
	case 0x03:
		reply = append(reply, 0x03, 3)
		reply = append(reply, []byte("foo")...)
		reply = append(reply, 0x00, 0x50)
	case 0x04:
		reply = append(reply, 0x04)
		reply = append(reply, net.ParseIP("::1").To16()...)
		reply = append(reply, 0x00, 0x50)
	default:
		reply = append(reply, 0x01, 127, 0, 0, 1, 0x00, 0x50)
	}
	if _, err := conn.Write(reply); err != nil {
		return
	}

	// On success, act as an echo server so the caller sees the tunnel is
	// usable and the bound address really was consumed.
	if s.replyCode == 0x00 {
		_, _ = io.Copy(conn, br)
	}
}

// dialThrough socks5Handshakes against the fake proxy and reports the error.
func dialThrough(t *testing.T, proxyURL, target string, user *url.Userinfo) error {
	t.Helper()
	conn, err := net.Dial("tcp", proxyURL[len("socks5://"):])
	if err != nil {
		t.Fatalf("dial the test proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return socks5Handshake(conn, user, target)
}

// The credentials path: the greeting must offer 0x02, the exchange must carry
// the URL's username and password verbatim, and the handshake must complete.
func TestSocks5HandshakeWithCredentials(t *testing.T) {
	srv := startSocks5(t, &socks5Server{wantAuth: 0x02, user: "alice", pass: "s3cr3t"})

	user := url.UserPassword("alice", "s3cr3t")
	if err := dialThrough(t, srv.url().String(), "example.com:443", user); err != nil {
		t.Fatalf("handshake with credentials: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.gotUser != "alice" || srv.gotPass != "s3cr3t" {
		t.Errorf("server saw user=%q pass=%q, want alice/s3cr3t", srv.gotUser, srv.gotPass)
	}
	// The greeting has to advertise both methods: a proxy that only offers
	// 0x00 cannot be told to authenticate.
	if len(srv.sawGreet) < 4 || srv.sawGreet[0] != 0x05 || srv.sawGreet[2] != 0x00 || srv.sawGreet[3] != 0x02 {
		t.Errorf("greeting = % x, want version 5 offering methods 0x00 and 0x02", srv.sawGreet)
	}
}

// A proxy that demands credentials must fail closed when the URL carries none,
// rather than proceeding unauthenticated.
func TestSocks5WantsCredentialsButNoneGiven(t *testing.T) {
	srv := startSocks5(t, &socks5Server{wantAuth: 0x02, user: "alice", pass: "s3cr3t"})
	err := dialThrough(t, srv.url().String(), "example.com:443", nil)
	if err == nil {
		t.Fatal("expected an error when the proxy asks for credentials")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("error = %v, want it to mention the missing credentials", err)
	}
}

// A rejected login is a hard failure: the router must fall through to another
// proxy, not send the request to a tunnel that was never established.
func TestSocks5AuthenticationRejected(t *testing.T) {
	srv := startSocks5(t, &socks5Server{wantAuth: 0x02, rejectAuth: true, user: "alice", pass: "s3cr3t"})
	err := dialThrough(t, srv.url().String(), "example.com:443", url.UserPassword("alice", "wrong"))
	if err == nil {
		t.Fatal("expected an error when the proxy rejects the credentials")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %v, want it to say the authentication was rejected", err)
	}
}

// A proxy that answers with no method we offered means the URL's credentials
// are wrong, or the proxy wants something this build cannot do.
func TestSocks5NoAcceptableAuthMethod(t *testing.T) {
	srv := startSocks5(t, &socks5Server{wantAuth: 0xFF})
	err := dialThrough(t, srv.url().String(), "example.com:443", nil)
	if err == nil {
		t.Fatal("expected an error for an unsupported auth method")
	}
	if !strings.Contains(err.Error(), "auth method") {
		t.Errorf("error = %v, want it to name the auth method problem", err)
	}
}

// socks5h resolves the target name at the proxy, so the name has to go out as a
// domain (ATYP 0x03) rather than being resolved locally into an address. This
// is the path that keeps a hostname-based provider working.
func TestSocks5DomainAddressType(t *testing.T) {
	for _, bind := range []byte{0x01, 0x03, 0x04} {
		srv := startSocks5(t, &socks5Server{bindAddrType: bind})
		if err := dialThrough(t, srv.url().String(), "api.provider.example:443", nil); err != nil {
			t.Fatalf("handshake to a domain target: %v", err)
		}
		srv.mu.Lock()
		atyp, addr := srv.gotATYP, srv.gotAddr
		srv.mu.Unlock()
		if atyp != 0x03 {
			t.Errorf("address type = 0x%02x, want 0x03 (domain)", atyp)
		}
		if addr != "api.provider.example:443" {
			t.Errorf("target = %q, want api.provider.example:443 verbatim", addr)
		}
	}
}

// A literal IPv4 target goes out as ATYP 0x01 with four bytes, not as the
// string: a proxy that is handed "1.2.3.4" as a domain name will look it up.
func TestSocks5IPv4AddressType(t *testing.T) {
	srv := startSocks5(t, &socks5Server{})
	if err := dialThrough(t, srv.url().String(), "1.2.3.4:8080", nil); err != nil {
		t.Fatalf("handshake to an IPv4 target: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.gotATYP != 0x01 {
		t.Errorf("address type = 0x%02x, want 0x01 (IPv4)", srv.gotATYP)
	}
	if srv.gotAddr != "1.2.3.4:8080" {
		t.Errorf("target = %q, want 1.2.3.4:8080", srv.gotAddr)
	}
}

// IPv6 targets go out as ATYP 0x04 with sixteen bytes.
func TestSocks5IPv6AddressType(t *testing.T) {
	srv := startSocks5(t, &socks5Server{})
	if err := dialThrough(t, srv.url().String(), "[2001:db8::1]:443", nil); err != nil {
		t.Fatalf("handshake to an IPv6 target: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.gotATYP != 0x04 {
		t.Errorf("address type = 0x%02x, want 0x04 (IPv6)", srv.gotATYP)
	}
	if srv.gotAddr != "[2001:db8::1]:443" {
		t.Errorf("target = %q, want [2001:db8::1]:443", srv.gotAddr)
	}
}

// The bound address in the reply has to be consumed off the wire, whatever form
// it arrives in. A byte left unread would be the first byte of the response
// body, which is the classic SOCKS5 corruption bug.
func TestSocks5DrainsBoundAddress(t *testing.T) {
	for _, atyp := range []byte{0x01, 0x03, 0x04} {
		srv := startSocks5(t, &socks5Server{bindAddrType: atyp})
		conn, err := net.Dial("tcp", srv.ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if err := socks5Handshake(conn, nil, "example.com:443"); err != nil {
			t.Errorf("bind address type 0x%02x: %v", atyp, err)
			_ = conn.Close()
			continue
		}
		// The tunnel is live: whatever we send must come back untouched.
		if _, err := conn.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
			t.Errorf("bind address type 0x%02x: write through the tunnel: %v", atyp, err)
		}
		buf := make([]byte, len("GET / HTTP/1.0\r\n\r\n"))
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Errorf("bind address type 0x%02x: the bound address was left on the wire (%v)", atyp, err)
		}
		_ = conn.Close()
	}
}

// A non-zero CONNECT status must abort with the reason attached: the reply
// status is the only explanation a user gets for a proxy that will not connect.
func TestSocks5ConnectFailureReportsStatus(t *testing.T) {
	for _, code := range []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08} {
		srv := startSocks5(t, &socks5Server{replyCode: code})
		err := dialThrough(t, srv.url().String(), "example.com:443", nil)
		if err == nil {
			t.Errorf("reply code 0x%02x: expected an error", code)
			continue
		}
		if want := socks5Status(code); !strings.Contains(err.Error(), want) {
			t.Errorf("reply code 0x%02x: error = %v, want it to say %q", code, err, want)
		}
	}
}

// socks5Status is user-facing text for each reply code, and codes outside the
// list must still produce something usable rather than an empty string.
func TestSocks5Status(t *testing.T) {
	for _, tc := range []struct {
		code byte
		want string
	}{
		{0x01, "general failure"},
		{0x02, "connection not allowed"},
		{0x03, "network unreachable"},
		{0x04, "host unreachable"},
		{0x05, "connection refused"},
		{0x06, "ttl expired"},
		{0x07, "command not supported"},
		{0x08, "address type not supported"},
	} {
		if got := socks5Status(tc.code); got != tc.want {
			t.Errorf("socks5Status(0x%02x) = %q, want %q", tc.code, got, tc.want)
		}
	}
	// An unknown code still has to say something a log can be read from.
	for _, code := range []byte{0x00, 0x09, 0xFF} {
		got := socks5Status(code)
		if got == "" {
			t.Errorf("socks5Status(0x%02x) returned an empty string", code)
		}
		if want := fmt.Sprintf("0x%02x", code); !strings.Contains(got, want) {
			t.Errorf("socks5Status(0x%02x) = %q, want it to include the hex code %q", code, got, want)
		}
	}
}

// A proxy answering with the wrong version is not a SOCKS5 server. Saying so
// beats reporting a mysterious read failure later.
func TestSocks5RejectsWrongVersion(t *testing.T) {
	// Answer the greeting with version 4.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		methods := make([]byte, hdr[1])
		_, _ = io.ReadFull(conn, methods)
		_, _ = conn.Write([]byte{0x04, 0x00})
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	err = socks5Handshake(conn, nil, "example.com:443")
	if err == nil {
		t.Fatal("expected an error for a wrong-version reply")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("error = %v, want it to mention the version mismatch", err)
	}
}

// Credentials longer than a byte cannot be encoded at all, and must be
// rejected rather than truncated into a different login.
func TestSocks5RejectsOverlongCredentials(t *testing.T) {
	srv := startSocks5(t, &socks5Server{wantAuth: 0x02})
	long := strings.Repeat("a", 256)
	err := dialThrough(t, srv.url().String(), "example.com:443", url.UserPassword(long, "pw"))
	if err == nil {
		t.Fatal("expected an error for an over-long username")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("error = %v, want it to say the credentials are too long", err)
	}
}

// A target without a port cannot be dialled, and the failure has to name the
// target rather than surfacing as a bare parse error from deep inside.
func TestSocks5RejectsTargetWithoutPort(t *testing.T) {
	srv := startSocks5(t, &socks5Server{})
	err := dialThrough(t, srv.url().String(), "example.com", nil)
	if err == nil {
		t.Fatal("expected an error for a target with no port")
	}
	if !strings.Contains(err.Error(), "example.com") {
		t.Errorf("error = %v, want it to name the target", err)
	}
}

// A hostname longer than 255 bytes cannot be length-prefixed, so it has to be
// refused up front.
func TestSocks5RejectsOverlongHostname(t *testing.T) {
	srv := startSocks5(t, &socks5Server{})
	err := dialThrough(t, srv.url().String(), strings.Repeat("h", 300)+".example:443", nil)
	if err == nil {
		t.Fatal("expected an error for an over-long hostname")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("error = %v, want it to say the hostname is too long", err)
	}
}

// An unknown address type in the reply is a malformed response; leaving it
// unread would corrupt the payload that follows.
func TestSocks5UnknownReplyAddressType(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(conn)
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(br, hdr); err != nil {
			return
		}
		methods := make([]byte, hdr[1])
		_, _ = io.ReadFull(br, methods)
		_, _ = conn.Write([]byte{0x05, 0x00})

		reqHead := make([]byte, 4)
		if _, err := io.ReadFull(br, reqHead); err != nil {
			return
		}
		if reqHead[3] == 0x03 {
			l := make([]byte, 1)
			_, _ = io.ReadFull(br, l)
			b := make([]byte, l[0])
			_, _ = io.ReadFull(br, b)
		} else {
			b := make([]byte, 4)
			_, _ = io.ReadFull(br, b)
		}
		pb := make([]byte, 2)
		_, _ = io.ReadFull(br, pb)
		// Success, but with an address type that does not exist.
		_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x09})
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	err = socks5Handshake(conn, nil, "example.com:443")
	if err == nil {
		t.Fatal("expected an error for an unknown reply address type")
	}
	if !strings.Contains(err.Error(), "address type") {
		t.Errorf("error = %v, want it to mention the address type", err)
	}
}

// The dial path has to build a working client for a socks5 URL and put the
// handshake in front of every connection it opens.
func TestNewProxyClientDialsThroughSocks5(t *testing.T) {
	srv := startSocks5(t, &socks5Server{})

	// A plain TCP echo server stands in for the target.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer target.Close()
	go func() {
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	client := NewProxyClient(srv.url(), 5*time.Second, 1)
	defer client.CloseIdleConnections()

	// Drive the client's dialer directly: a full HTTP exchange would need the
	// fake proxy to also speak HTTP.
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", client.Transport)
	}
	conn, err := tr.DialContext(context.Background(), "tcp", target.Addr().String())
	if err != nil {
		t.Fatalf("dial through the socks5 proxy: %v", err)
	}
	defer conn.Close()

	const msg = "tunnelled"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through the tunnel: %v", err)
	}
	if string(buf) != msg {
		t.Errorf("echoed %q, want %q", buf, msg)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.gotAddr != target.Addr().String() {
		t.Errorf("proxy was asked for %q, want %q", srv.gotAddr, target.Addr().String())
	}
}

// The transport cache is bounded, so a rotating list cannot retain a transport
// (and an idle connection pool) for every proxy it has ever seen.
//
// The cache rests at maxCachedTransports+1 rather than exactly the cap:
// NewTransport evicts before it inserts, so the entry that tips it over the
// limit is only dropped on the following call. That is a harmless off-by-one
// for a bound, and the bound is the point of this test.
func TestTransportCacheIsBounded(t *testing.T) {
	resetTransportCache()

	const created = maxCachedTransports * 5
	for i := range created {
		u, _ := url.Parse(fmt.Sprintf("http://10.%d.%d.%d:8080", i/65536, (i/256)%256, i%256))
		NewTransport(u, time.Duration(i+1)*time.Second, 1, false)
	}

	transportCache.mu.Lock()
	got := len(transportCache.m)
	transportCache.mu.Unlock()

	if got > maxCachedTransports+1 {
		t.Errorf("cache holds %d transports after %d distinct keys, want at most %d",
			got, created, maxCachedTransports+1)
	}
}

// resetTransportCache empties the package-level cache so a test starts from a
// known state; the real one is process-wide and shared between tests.
func resetTransportCache() {
	transportCache.mu.Lock()
	transportCache.m = map[string]*cachedTransport{}
	transportCache.mu.Unlock()
}

// Identical settings must share one transport, and the timeout and TLS flag are
// part of the key: two providers that differ on either must not share one.
func TestTransportCacheKeyIncludesEverySetting(t *testing.T) {
	resetTransportCache()

	u, _ := url.Parse("http://10.1.1.1:8080")

	a := NewTransport(u, 5*time.Second, 1, false)
	b := NewTransport(u, 5*time.Second, 1, false)
	if a != b {
		t.Error("identical settings should reuse the cached transport")
	}
	if c := NewTransport(u, 6*time.Second, 1, false); c == a {
		t.Error("a different timeout must not share a transport")
	}
	if d := NewTransport(u, 5*time.Second, 1, true); d == a {
		t.Error("a different skipVerify must not share a transport")
	}
	if e := NewTransport(u, 5*time.Second, 2, false); e == a {
		t.Error("a different connection budget must not share a transport")
	}
}

// An http proxy URL is handed to http.Transport rather than the SOCKS5 dialer.
func TestNewTransportWiresProxyScheme(t *testing.T) {
	resetTransportCache()

	httpProxy, _ := url.Parse("http://10.2.2.2:3128")
	tr := NewTransport(httpProxy, time.Second, 1, false)
	if tr.Proxy == nil {
		t.Error("an http proxy URL should set Transport.Proxy")
	}
	if tr.DialContext == nil {
		t.Error("transport has no dialer")
	}

	socksProxy, _ := url.Parse("socks5://10.3.3.3:1080")
	tr2 := NewTransport(socksProxy, time.Second, 1, false)
	if tr2.Proxy != nil {
		t.Error("a socks5 proxy must be dialled by hand, not via Transport.Proxy")
	}
}

// PruneTransportCache exists so a refresh can drop the transports built for
// proxies that fell out of the list.
func TestPruneTransportCacheEmptiesWhenIdle(t *testing.T) {
	resetTransportCache()

	u, _ := url.Parse("http://10.4.4.4:8080")
	NewTransport(u, time.Second, 1, false)

	transportCache.mu.Lock()
	transportCache.m["stale"] = &cachedTransport{
		tr:       &http.Transport{},
		lastUsed: time.Now().Add(-2 * transportCacheTTL),
	}
	transportCache.mu.Unlock()

	PruneTransportCache()

	transportCache.mu.Lock()
	_, staleStill := transportCache.m["stale"]
	remaining := len(transportCache.m)
	transportCache.mu.Unlock()

	if staleStill {
		t.Error("PruneTransportCache kept a transport past its idle TTL")
	}
	if remaining != 1 {
		t.Errorf("cache holds %d transports, want only the just-used one", remaining)
	}
}
