package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

const (
	maxLogSize = 10 << 20 // rotate past 10 MB
	oldLogs    = 3        // keep this many rotated files
	// subscriberBuffer is how many lines a stream subscriber may lag before it is dropped.
	subscriberBuffer = 256
)

// logFile appends to path, rotating before a write that would take it past
// max, and copies every write to the stream subscribers. slog's handlers write
// one record per Write, so a record is never split across files.
type logFile struct {
	path string
	max  int64

	mu     sync.Mutex
	f      *os.File // nil until the first write and after a failed reopen
	size   int64
	subs   map[chan []byte]struct{}
	closed bool
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, os.ErrClosed
	}
	line := bytes.Clone(p) // slog reuses its buffer
	for ch := range l.subs {
		select {
		case ch <- line:
		default: // a slow or gone subscriber never blocks logging: its stream ends
			delete(l.subs, ch)
			close(ch)
		}
	}
	if l.f != nil && l.size > 0 && l.size+int64(len(p)) > l.max {
		l.f.Close()
		l.f = nil
		for i := oldLogs - 1; i >= 1; i-- {
			os.Rename(l.path+"."+strconv.Itoa(i), l.path+"."+strconv.Itoa(i+1))
		}
		os.Rename(l.path, l.path+".1")
	}
	if l.f == nil {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return 0, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return 0, err
		}
		l.f, l.size = f, fi.Size()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// follow returns the last n lines of the current file and subscribes to every
// line written after them; cancel ends the subscription.
func (l *logFile) follow(n int) (tail []byte, lines <-chan []byte, cancel func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// shortcut: reads the whole current file (at most max bytes) under the lock; seek from the end if that ever shows up.
	raw, err := os.ReadFile(l.path)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, nil, err
	}
	start := len(raw)
	for ; start > 0 && n > 0; n-- {
		start = bytes.LastIndexByte(raw[:start-1], '\n') + 1
	}
	ch := make(chan []byte, subscriberBuffer)
	if l.subs == nil {
		l.subs = map[chan []byte]struct{}{}
	}
	if l.closed {
		close(ch)
	} else {
		l.subs[ch] = struct{}{}
	}
	return raw[start:], ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if _, ok := l.subs[ch]; ok {
			delete(l.subs, ch)
			close(ch)
		}
	}, nil
}

// Close closes the file and ends every stream; later writes fail.
func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for ch := range l.subs {
		delete(l.subs, ch)
		close(ch)
	}
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}
