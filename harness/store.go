package harness

import "context"

type EventStore interface {
	CreateStream(context.Context, StreamDescriptor) error
	Append(context.Context, int64, ...PendingEvent) ([]Event, error)
	Load(context.Context, int64, int) ([]Event, error)
	LoadStream(context.Context, string, int64, int) ([]Event, error)
	Head(context.Context) (int64, error)
	SaveCheckpoint(context.Context, Checkpoint) error
	LoadCheckpoint(context.Context, string, string) (*Checkpoint, error)
	SaveCommandResult(context.Context, CommandResult) error
	GetCommandResult(context.Context, string) (*CommandResult, error)
	Close() error
}

type BlobStore interface {
	PutBlob(context.Context, string, []byte) (string, error)
	GetBlob(context.Context, string) ([]byte, string, error)
}

type ConsumerStore interface {
	ConsumerOffset(context.Context, string) (int64, error)
	SaveConsumerOffset(context.Context, string, int64) error
}

type Projector interface {
	Name() string
	Version() int
	Reset()
	Apply(Event) error
	MarshalState() ([]byte, error)
	UnmarshalState([]byte) error
}

func Replay(ctx context.Context, store EventStore, projector Projector, afterSeq int64) (int64, error) {
	const pageSize = 500
	last := afterSeq
	for {
		events, err := store.Load(ctx, last, pageSize)
		if err != nil {
			return last, err
		}
		for _, event := range events {
			if err := projector.Apply(event); err != nil {
				return last, err
			}
			last = event.Seq
		}
		if len(events) < pageSize {
			return last, nil
		}
	}
}
