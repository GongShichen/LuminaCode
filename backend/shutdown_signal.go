package backend

import "sync"

type ShutdownSignal struct {
	once sync.Once
	done chan struct{}
}

func NewShutdownSignal() *ShutdownSignal {
	return &ShutdownSignal{done: make(chan struct{})}
}

func (s *ShutdownSignal) Request() {
	if s == nil {
		return
	}
	s.once.Do(func() { close(s.done) })
}

func (s *ShutdownSignal) Done() <-chan struct{} {
	return s.done
}
