package router

// withRecovery's contract: a panic is answered with a 500 only while the
// client has not been answered yet. Handlers panic in practice mid-stream -
// during the io.Copy of a completion - and appending a JSON error object
// there would corrupt the response already on the wire.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"noderouter/logx"
)

func recoveryTestHandler(h http.HandlerFunc) http.Handler { return withRecovery(logx.New(false), h) }

func TestRecoveryWrites500BeforeHeaders(t *testing.T) {
	h := recoveryTestHandler(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal error") {
		t.Errorf("body = %q, want the JSON error object", rec.Body.String())
	}
}

// A panic once the response has started must not append anything: the client
// is mid-stream and would see a JSON object spliced into its events.
func TestRecoveryDoesNotAppendAfterHeaders(t *testing.T) {
	const written = "data: {\"partial\":true}\n\n"
	h := recoveryTestHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, written); err != nil {
			return
		}
		panic("mid-stream")
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/chat/completions", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d after headers were sent, want the handler's 200", rec.Code)
	}
	if got := rec.Body.String(); got != written {
		t.Errorf("body = %q, want exactly what the handler wrote (no error JSON appended)", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want the handler's header left alone", ct)
	}
}

// http.ErrAbortHandler is the handler's own request to stop quietly, the way
// net/http treats it: no response, no error log.
func TestRecoveryErrAbortHandlerIsSilent(t *testing.T) {
	h := recoveryTestHandler(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Body.Len() != 0 || rec.Code != http.StatusOK {
		t.Errorf("status = %d body = %q, want nothing written at all", rec.Code, rec.Body.String())
	}
}

// The wrapper sits between the handlers and the real ResponseWriter; flushing
// has to keep reaching the real one through Unwrap, or every stream stalls.
func TestRecoveryFlushReachesUnderlyingWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	h := recoveryTestHandler(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "chunk"); err != nil {
			t.Errorf("write through the recovery wrapper: %v", err)
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through the recovery wrapper: %v", err)
		}
	})
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !rec.Flushed {
		t.Errorf("flush did not reach the underlying ResponseWriter")
	}
}
