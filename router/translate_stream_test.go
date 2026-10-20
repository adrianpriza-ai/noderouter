package router

// The streaming translator's write path. The translation itself is covered
// end to end from tests/; this one needs the reader and the writer directly,
// because the behaviour under test is what happens when the writer starts
// failing while the reader never ends.

import (
	"errors"
	"testing"
	"time"
)

// errClientGone is what a client that hung up mid-stream looks like to the
// writer.
var errClientGone = errors.New("client connection is gone")

// budgetWriter accepts its first budget bytes and fails every write after,
// the way a ResponseWriter does once the client on the other end is gone.
type budgetWriter struct{ budget int }

func (w *budgetWriter) Write(p []byte) (int, error) {
	if len(p) > w.budget {
		return 0, errClientGone
	}
	w.budget -= len(p)
	return len(p), nil
}

// endlessEvents is an upstream that never finishes: it answers every read
// with one complete Responses event and counts the reads, so the test can see
// how long a client that stopped accepting writes keeps the translator busy.
type endlessEvents struct{ reads int }

const deltaBlock = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"

func (e *endlessEvents) Read(p []byte) (int, error) {
	e.reads++
	// One whole event per read; the copy loop's buffer is far larger than
	// this, so nothing is ever split across reads.
	return copy(p, deltaBlock), nil
}

// TestTranslateStreamStopsWhenTheClientWriteFails checks that a failed write
// to the client ends the translation. The translator used to ignore every
// write error, so a client that had already gone left it parsing and
// translating an endless upstream until the upstream itself finished - and
// the translated-stream error path could never be reached.
func TestTranslateStreamStopsWhenTheClientWriteFails(t *testing.T) {
	reader := &endlessEvents{}
	// A few events fit, then every write fails.
	writer := &budgetWriter{budget: 512}

	// Client speaks chat, upstream speaks Responses: events in, chat chunks
	// out, exactly as the copy loop drives it.
	tr := &translation{from: wireChat, to: wireResponses, streaming: true}

	done := make(chan error, 1)
	go func() {
		done <- tr.translateStream(reader, writer)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errClientGone) {
			t.Fatalf("translateStream error = %v, want the failed client write to surface", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("translateStream was still translating after every write to the client had failed")
	}

	// One event per read, so this is roughly the number of events the dead
	// client was still being translated for before the loop gave up.
	if reader.reads > 16 {
		t.Errorf("read %d events for a client that stopped accepting writes; want a handful", reader.reads)
	}
}
