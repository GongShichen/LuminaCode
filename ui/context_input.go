package ui

import (
	"context"
	"io"
)

type inputRead struct {
	data []byte
	err  error
}

// contextInput keeps at most one read in flight. Cancellation releases the UI
// without closing stdin. A late read is retained for the next input operation,
// rather than stealing that operation's input or touching its bufio.Reader.
// Like bufio.Reader, it is owned by the UI loop, not used concurrently.
type contextInput struct {
	source  io.Reader
	ctx     context.Context
	pending chan inputRead
	buffer  []byte
	err     error
}

func (r *contextInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	if len(r.buffer) == 0 && r.err == nil {
		if r.pending == nil {
			r.pending = make(chan inputRead, 1)
			pending := r.pending
			size := len(p)
			go func() {
				data := make([]byte, size)
				n, err := r.source.Read(data)
				pending <- inputRead{data[:n], err}
			}()
		}
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case result := <-r.pending:
			r.pending = nil
			r.buffer, r.err = result.data, result.err
		}
	}
	n := copy(p, r.buffer)
	r.buffer = r.buffer[n:]
	if len(r.buffer) != 0 {
		return n, nil
	}
	err := r.err
	r.err = nil
	return n, err
}
