package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failOnWriteWriter is a ResponseWriter+Flusher whose writes always fail,
// simulating a client that went away mid-event.
type failOnWriteWriter struct {
	header http.Header
}

func (f *failOnWriteWriter) Header() http.Header { return f.header }
func (f *failOnWriteWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}
func (f *failOnWriteWriter) WriteHeader(int) {}
func (f *failOnWriteWriter) Flush()          {}

// TestSSEStreamSendWritesFramedEvent pins the wire format: one event line,
// one data line, a blank terminator, and a flush.
func TestSSEStreamSendWritesFramedEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := &SSEStream{w: rec, f: rec}

	require.True(t, stream.Send("change", "payload"))
	assert.Equal(t, "event: change\ndata: payload\n\n", rec.Body.String())
	assert.True(t, rec.Flushed)
}

// TestSSEStreamSendJSONRoundTrip: values marshal into a single data line.
func TestSSEStreamSendJSONRoundTrip(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := &SSEStream{w: rec, f: rec}

	payload := struct {
		ID int  `json:"id"`
		OK bool `json:"ok"`
	}{ID: 7, OK: true}
	require.True(t, stream.SendJSON("session", payload))
	assert.Equal(t, "event: session\ndata: {\"id\":7,\"ok\":true}\n\n", rec.Body.String())
	assert.True(t, rec.Flushed)
}

// TestSSEStreamSendFailsOnWriteError: a dead client makes Send return false
// so callers can drop the subscriber instead of blocking on it.
func TestSSEStreamSendFailsOnWriteError(t *testing.T) {
	w := &failOnWriteWriter{header: http.Header{}}
	stream := &SSEStream{w: w, f: w}
	assert.False(t, stream.Send("change", "payload"))
}

// TestSSEStreamSendJSONMarshalFailureWritesNothing: an unmarshalable value
// must return false without emitting a partial frame.
func TestSSEStreamSendJSONMarshalFailureWritesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := &SSEStream{w: rec, f: rec}

	assert.False(t, stream.SendJSON("change", func() {}))
	assert.Empty(t, rec.Body.String())
}

// TestSSEStreamSendMultilineDataSplitsIntoDataLines pins SSE multi-line
// data framing: each source line gets its own "data:" line so the client
// receives the complete payload.
func TestSSEStreamSendMultilineDataSplitsIntoDataLines(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := &SSEStream{w: rec, f: rec}

	require.True(t, stream.Send("change", "line one\nline two"))
	assert.Equal(t, "event: change\ndata: line one\ndata: line two\n\n", rec.Body.String())
}
