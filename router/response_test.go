package router

// writeResponseTr is the only path an upstream answer takes to the client, so
// two things about it are worth pinning: a plain passthrough is byte-for-byte
// the upstream's, and a request that crossed surfaces gets the rewrite while an
// error does not (the provider's own refusal belongs to the provider).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"noderouter/config"
	"noderouter/logx"
	"noderouter/proxy"
)

func responseTestServer() *Server {
	cfg := &config.Config{}
	cfg.ApplyDefaults()
	return NewServer(cfg, nil, logx.New(false))
}

func writeTr(t *testing.T, srv *Server, resp *http.Response, tr *translation) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.writeResponseTr(rec, resp, proxy.ProxyEntry{}, false, tr)
	return rec
}

func upstream(body, contentType string) *http.Response {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A plain answer passes through untouched: status, body and headers, because
// this is the path every non-translated request takes.
func TestWriteResponseTrPlainPassthrough(t *testing.T) {
	srv := responseTestServer()
	resp := upstream(`{"ok":true}`, "application/json")
	resp.Header.Set("X-Request-Id", "abc123")

	rec := writeTr(t, srv, resp, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Errorf("body = %q, want the upstream's bytes", got)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "abc123" {
		t.Errorf("X-Request-Id = %q, want it copied", got)
	}
}

// An error answer is the provider's own, in the provider's own dialect, even
// when the request crossed surfaces: the client asked in one shape and was
// refused in another, and translating the refusal would be a message in a
// dialect nobody asked for.
func TestWriteResponseTrLeavesErrorsUntranslated(t *testing.T) {
	srv := responseTestServer()
	tr := &translation{from: wireChat, to: wireResponses}

	resp := upstream(`{"error":{"message":"model not found","type":"invalid_request_error"}}`,
		"application/json")
	resp.StatusCode = http.StatusNotFound

	rec := writeTr(t, srv, resp, tr)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want the upstream's 404", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "model not found") {
		t.Errorf("body = %q, want the provider's error verbatim", got)
	}
	if n := srv.errors.Load(); n != 1 {
		t.Errorf("error counter = %d after one 404, want 1", n)
	}
}

// A translated 2xx body has a different length from the upstream's, so the
// length headers describing the old one must be gone and the new one correct.
func TestWriteResponseTrFixesLengthOnATranslatedBody(t *testing.T) {
	srv := responseTestServer()
	tr := &translation{from: wireChat, to: wireResponses}

	// A responses-shaped answer asking to be read as chat.
	respBody := `{"id":"r1","object":"response","status":"completed",` +
		`"output":[{"type":"message","role":"assistant",` +
		`"content":[{"type":"output_text","text":"hi there"}]}]}`
	resp := upstream(respBody, "application/json")
	resp.Header.Set("Content-Length", "999999")

	rec := writeTr(t, srv, resp, tr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "output_text") || strings.Contains(body, `"object":"response"`) {
		t.Errorf("body = %q, want it rewritten into the chat shape", body)
	}
	if !strings.Contains(body, "hi there") {
		t.Errorf("body = %q, want the upstream's text carried across", body)
	}
	// The header we inherited described the old body; the recorder builds its
	// own from what was written, so assert on the bytes actually sent.
	if got := rec.Body.Len(); got != len(body) {
		t.Errorf("wrote %d bytes but the body is %d", got, len(body))
	}
	// The recorder carries whatever Content-Length was written; the point is
	// that it matches the rewritten body, not the upstream's 999999.
	if n := rec.Result().Header.Get("Content-Length"); n != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %d (the rewritten body)", n, len(body))
	}
}

// A stream is translated as it arrives rather than buffered: the client is
// waiting on it and buffering a completion defeats the point.
func TestWriteResponseTrTranslatesAStream(t *testing.T) {
	srv := responseTestServer()
	tr := &translation{from: wireChat, to: wireResponses}

	stream := `data: {"type":"response.output_text.delta","delta":"hi"}

data: {"type":"response.completed","response":{"id":"r1","object":"response","status":"completed"}}

data: [DONE]

`
	resp := upstream(stream, "text/event-stream")

	rec := writeTr(t, srv, resp, tr)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want the event stream the client expects", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hi") {
		t.Errorf("body = %q, want the streamed text", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("body = %q, want the stream terminated for the chat client", body)
	}
}

// The proxy actually used is reported back to the client, so a user can see
// which hop answered. Omitted when the answer came direct.
func TestWriteResponseTrReportsTheProxy(t *testing.T) {
	srv := responseTestServer()

	rec := httptest.NewRecorder()
	srv.writeResponseTr(rec, upstream(`{}`, "application/json"),
		proxy.ProxyEntry{URL: "http://proxy.example:8080"}, true, nil)
	if got := rec.Header().Get("X-Noderouter-Proxy"); got != "http://proxy.example:8080" {
		t.Errorf("X-Noderouter-Proxy = %q, want the hop that answered", got)
	}

	rec = httptest.NewRecorder()
	srv.writeResponseTr(rec, upstream(`{}`, "application/json"),
		proxy.ProxyEntry{URL: "http://proxy.example:8080"}, false, nil)
	if got := rec.Header().Get("X-Noderouter-Proxy"); got != "" {
		t.Errorf("X-Noderouter-Proxy = %q on a direct answer, want it absent", got)
	}
}

// Hop-by-hop headers describe one connection, not the next one, so they must
// not be relayed in either direction.
func TestWriteResponseTrDropsHopHeaders(t *testing.T) {
	srv := responseTestServer()
	resp := upstream(`{}`, "application/json")
	resp.Header.Set("Transfer-Encoding", "chunked")
	resp.Header.Set("Connection", "keep-alive")
	resp.Header.Set("Proxy-Authenticate", "Basic")

	rec := writeTr(t, srv, resp, nil)
	for _, h := range []string{"Transfer-Encoding", "Connection", "Proxy-Authenticate"} {
		if got := rec.Header().Get(h); got != "" {
			t.Errorf("%s = %q, want it dropped", h, got)
		}
	}
}

// The response body is always closed, whatever happens to the write: an
// unclosed body holds the upstream connection until it times out.
func TestWriteResponseTrClosesTheUpstreamBody(t *testing.T) {
	srv := responseTestServer()
	tracked := &trackedBody{Reader: strings.NewReader(`{}`)}
	resp := upstream("", "application/json")
	resp.Body = tracked

	writeTr(t, srv, resp, nil)
	if !tracked.closed {
		t.Error("the upstream body was not closed")
	}
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Replace(strings.Repeat("", 0)+fmtInt(n), " ", "", -1))
}

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
