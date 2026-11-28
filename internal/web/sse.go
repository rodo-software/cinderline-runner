package web

import "net/http"

// sse writes server-sent events to an answer.
type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// newSSE sets w's headers for an event stream.
func newSSE(w http.ResponseWriter) sse {
	w.Header().Set("Content-Type", "text/event-stream")
	return sse{w, http.NewResponseController(w)}
}

// send writes one event of data, which must hold no newline (JSON from
// json.Marshal holds none), and flushes it.
func (s sse) send(data []byte) error {
	if _, err := s.w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
		return err
	}
	return s.rc.Flush()
}

// ping writes a comment line, so a dead connection is noticed.
func (s sse) ping() error {
	if _, err := s.w.Write([]byte(": ping\n\n")); err != nil {
		return err
	}
	return s.rc.Flush()
}

// done writes the event named done, with empty data: an event without a
// data line is not dispatched.
func (s sse) done() error {
	if _, err := s.w.Write([]byte("event: done\ndata:\n\n")); err != nil {
		return err
	}
	return s.rc.Flush()
}
