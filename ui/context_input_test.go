package ui

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

type notifyingInput struct {
	io.Reader
	entered chan struct{}
	once    sync.Once
}

func (r *notifyingInput) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	return r.Reader.Read(p)
}

func TestNativePermissionCancellationPreservesNextInput(t *testing.T) {
	for _, kind := range []string{"terminal", "fullscreen"} {
		t.Run(kind, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			source := &notifyingInput{Reader: reader, entered: make(chan struct{})}
			var ask func(context.Context, any, bool) string
			var next func() (string, bool)
			if kind == "terminal" {
				backend := NewTerminalRendererBackend(source, io.Discard, io.Discard)
				ask = backend.AskPermissionContext
				next = func() (string, bool) { return backend.GetInput(nil) }
			} else {
				backend := NewFullscreenRendererBackend(source, io.Discard, io.Discard)
				ask = backend.AskPermissionContext
				next = func() (string, bool) { return backend.GetInput(nil) }
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan string, 1)
			go func() { done <- ask(ctx, map[string]any{"name": "run_shell"}, true) }()
			select {
			case <-source.entered:
			case <-time.After(time.Second):
				t.Fatal("permission input did not start")
			}
			cancel()
			select {
			case answer := <-done:
				if answer != "deny" {
					t.Fatal("cancelled modal accepted input")
				}
			case <-time.After(time.Second):
				t.Fatal("native modal did not cancel")
			}
			// A read that was already blocked may finish after cancellation. Those
			// bytes must feed the next input, never revive the old permission.
			written := make(chan error, 1)
			go func() { _, err := io.WriteString(writer, "next request\n"); written <- err }()
			if text, ok := next(); !ok || text != "next request" {
				t.Fatalf("next input lost: %q, %v", text, ok)
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
		})
	}
}
