package proxy

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Transport construction. Everything here is stdlib: http.Transport handles
// http/https proxies natively, and SOCKS5 is dialled by hand below so the
// project needs no third-party modules.

// Cached proxies each keep their own *http.Transport with idle connection
// pools, so an unbounded cache leaks one pool per proxy URL forever — and a
// rotating list refreshes that set regularly. The cache is therefore bounded:
// entries unused for transportCacheTTL are closed and dropped, and past
// maxCachedTransports the least-recently-used ones are evicted.
var transportCache = struct {
	mu sync.Mutex
	m  map[string]*cachedTransport
}{m: map[string]*cachedTransport{}}

type cachedTransport struct {
	tr       *http.Transport
	lastUsed time.Time
}

const (
	transportCacheTTL   = 5 * time.Minute
	maxCachedTransports = 64
)

// NewProxyClient returns a client that sends its traffic through proxy.
// It handles http, https and socks5 proxy URLs.
func NewProxyClient(proxy *url.URL, timeout time.Duration, maxConns int) *http.Client {
	return newProxyClientTLS(proxy, timeout, maxConns, false)
}

func newProxyClientTLS(proxy *url.URL, timeout time.Duration, maxConns int, skipVerify bool) *http.Client {
	tr := NewTransport(proxy, timeout, maxConns, skipVerify)
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func NewTransport(proxy *url.URL, timeout time.Duration, maxConns int, skipVerify bool) *http.Transport {
	// The timeout, TLS setting and the connection budget are part of the
	// key: two providers that differ on any must not share a transport.
	key := fmt.Sprintf("direct|%s|%d|%v", timeout, maxConns, skipVerify)
	if proxy != nil {
		key = fmt.Sprintf("%s|%s|%d|%v", proxy.String(), timeout, maxConns, skipVerify)
	}

	transportCache.mu.Lock()
	defer transportCache.mu.Unlock()
	evictTransportsLocked(time.Now())

	if cached, ok := transportCache.m[key]; ok {
		cached.lastUsed = time.Now()
		return cached.tr
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: skipVerify},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
	}
	if maxConns > 0 {
		tr.MaxConnsPerHost = maxConns
	}
	if proxy != nil {
		switch proxy.Scheme {
		case "socks5", "socks5h":
			tr.DialContext = socks5DialContext(proxy, dialer)
		default:
			p := *proxy
			tr.Proxy = http.ProxyURL(&p)
		}
	}
	transportCache.m[key] = &cachedTransport{tr: tr, lastUsed: time.Now()}
	return tr
}

// evictTransportsLocked drops idle-expired entries and trims to the LRU cap,
// closing idle connections on the way out. Caller must hold the cache mutex.
func evictTransportsLocked(now time.Time) {
	for key, e := range transportCache.m {
		if now.Sub(e.lastUsed) > transportCacheTTL {
			e.tr.CloseIdleConnections()
			delete(transportCache.m, key)
		}
	}
	for len(transportCache.m) > maxCachedTransports {
		var oldestKey string
		var oldest time.Time
		first := true
		for key, e := range transportCache.m {
			if first || e.lastUsed.Before(oldest) {
				oldestKey, oldest, first = key, e.lastUsed, false
			}
		}
		if e := transportCache.m[oldestKey]; e != nil {
			e.tr.CloseIdleConnections()
		}
		delete(transportCache.m, oldestKey)
	}
}

// PruneTransportCache evicts every entry past the idle TTL and closes its
// idle connections. The proxy refresh calls this so transports built for
// proxies that fell out of the list do not linger.
func PruneTransportCache() {
	transportCache.mu.Lock()
	defer transportCache.mu.Unlock()
	evictTransportsLocked(time.Now())
}

// socks5DialContext tunnels a connection through a SOCKS5 proxy with the
// CONNECT command (no auth, or username/password when the URL carries one).
func socks5DialContext(proxy *url.URL, fallback *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		dial := fallback.DialContext
		if dial == nil {
			dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				return net.Dial(network, address)
			}
		}
		conn, err := dial(ctx, network, proxy.Host)
		if err != nil {
			return nil, fmt.Errorf("socks5 dial %s: %w", proxy.Host, err)
		}
		// A proxy hostname may need resolving too; socks5h resolves remotely,
		// which is what we want here since the target is the provider.
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		if err := socks5Handshake(conn, proxy.User, addr); err != nil {
			conn.Close()
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

func socks5Handshake(conn net.Conn, user *url.Userinfo, target string) error {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("socks5 target %q: %w", target, err)
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return fmt.Errorf("socks5 port %q: %w", portStr, err)
	}

	methods := []byte{0x00}
	if user != nil {
		methods = append(methods, 0x02)
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 greeting: %w", err)
	}
	if reply[0] != 0x05 {
		return fmt.Errorf("socks5: unexpected version %d", reply[0])
	}
	switch reply[1] {
	case 0x00:
	case 0x02:
		if user == nil {
			return errors.New("socks5 proxy wants credentials but none were given")
		}
		pass, _ := user.Password()
		u, p := user.Username(), pass
		if len(u) > 255 || len(p) > 255 {
			return errors.New("socks5: credentials too long")
		}
		auth := []byte{0x01, byte(len(u))}
		auth = append(auth, u...)
		auth = append(auth, byte(len(p)))
		auth = append(auth, p...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		authReply := make([]byte, 2)
		if _, err := io.ReadFull(conn, authReply); err != nil {
			return err
		}
		if authReply[1] != 0x00 {
			return errors.New("socks5: authentication rejected")
		}
	default:
		return fmt.Errorf("socks5: no acceptable auth method (reply 0x%02x)", reply[1])
	}

	// CONNECT request.
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("socks5: hostname too long")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	req = append(req, portBytes[:]...)

	if _, err := conn.Write(req); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("socks5 connect: %w", err)
	}
	if head[0] != 0x05 {
		return fmt.Errorf("socks5: bad reply version %d", head[0])
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: %s", socks5Status(head[1]))
	}
	// Drain the bound address so the stream is left at the payload.
	switch head[3] {
	case 0x01:
		_, err = io.CopyN(io.Discard, conn, 4+2)
	case 0x04:
		_, err = io.CopyN(io.Discard, conn, 16+2)
	case 0x03:
		n := make([]byte, 1)
		if _, err = io.ReadFull(conn, n); err == nil {
			_, err = io.CopyN(io.Discard, conn, int64(n[0])+2)
		}
	default:
		err = fmt.Errorf("socks5: unknown address type %d", head[3])
	}
	return err
}

func socks5Status(code byte) string {
	switch code {
	case 0x01:
		return "general failure"
	case 0x02:
		return "connection not allowed"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "ttl expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("error 0x%02x", code)
	}
}
