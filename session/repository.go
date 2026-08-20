package session

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"LuminaCode/harness"
)

const LocalTenantID = "local"

var ErrRuntimeNotFound = errors.New("session runtime not found")

type RuntimeOpenOptions struct {
	CreateIfMissing bool
	ReadOnly        bool
	FenceToken      int64
	CWD             string
}

type RuntimeInfo struct {
	TenantID   string
	SessionID  string
	CWD        string
	Status     string
	FenceToken int64
}

type OutboxRecord struct {
	ID        int64
	TenantID  string
	SessionID string
	Seq       int64
	Event     harness.Event
}

// SessionRepository is the shared session catalog and runtime-store factory used by
// both local SQLite and clustered PostgreSQL deployments.
type SessionRepository interface {
	OpenRuntime(context.Context, string, string, RuntimeOpenOptions) (*RuntimeLoad, error)
	Exists(context.Context, string, string) (bool, error)
	RuntimeInfo(context.Context, string, string) (RuntimeInfo, error)
	ResolveParentSession(context.Context, string, string) (string, error)
	LoadEvents(context.Context, string, string, int64, int) ([]harness.Event, int64, error)
	Health(context.Context) error
	ClaimOutbox(context.Context, string, int, time.Duration) ([]OutboxRecord, error)
	MarkOutboxPublished(context.Context, string, []int64) error
	ListSessions(context.Context, string) ([]Meta, error)
	Pin(context.Context, string, string, bool) (*Meta, error)
	UpdateMetaProjection(context.Context, string, string, int64, int, int) error
	Close() error
}

type LocalRepository struct {
	store *Store
}

func NewLocalRepository(store *Store) *LocalRepository {
	return &LocalRepository{store: store}
}

func (r *LocalRepository) Store() *Store { return r.store }

func (r *LocalRepository) OpenRuntime(ctx context.Context, tenantID, sessionID string,
	options RuntimeOpenOptions) (*RuntimeLoad, error) {
	if err := requireLocalTenant(tenantID); err != nil {
		return nil, err
	}
	if options.ReadOnly {
		return nil, errors.New("read-only local runtime handles are not implemented")
	}
	if !options.CreateIfMissing {
		exists, err := r.Exists(ctx, tenantID, sessionID)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, os.ErrNotExist
		}
	}
	return r.store.OpenRuntime(ctx, sessionID)
}

func (r *LocalRepository) Exists(_ context.Context, tenantID, sessionID string) (bool, error) {
	if err := requireLocalTenant(tenantID); err != nil {
		return false, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return false, nil
	}
	if _, err := os.Stat(RuntimeJournalPath(r.store.dir, sessionID)); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return r.store.LoadState(sessionID) != nil || len(r.store.Load(sessionID)) > 0, nil
}

func (r *LocalRepository) RuntimeInfo(ctx context.Context, tenantID, sessionID string) (RuntimeInfo, error) {
	if err := requireLocalTenant(tenantID); err != nil {
		return RuntimeInfo{}, err
	}
	exists, err := r.Exists(ctx, tenantID, sessionID)
	if err != nil {
		return RuntimeInfo{}, err
	}
	if !exists {
		return RuntimeInfo{}, os.ErrNotExist
	}
	return RuntimeInfo{TenantID: LocalTenantID, SessionID: sessionID}, nil
}

func (r *LocalRepository) ResolveParentSession(_ context.Context, tenantID, streamID string) (string, error) {
	if err := requireLocalTenant(tenantID); err != nil {
		return "", err
	}
	for _, meta := range r.store.ListSessions() {
		journal, err := OpenRuntimeJournal(context.Background(), r.store.dir, meta.SessionID)
		if err != nil {
			continue
		}
		var found int
		err = journal.db.QueryRow(`SELECT COUNT(*) FROM streams WHERE stream_id=?`, streamID).Scan(&found)
		_ = journal.Close()
		if err == nil && found > 0 {
			return meta.SessionID, nil
		}
	}
	return "", os.ErrNotExist
}

func (r *LocalRepository) LoadEvents(ctx context.Context, tenantID, sessionID string, afterSeq int64,
	limit int) ([]harness.Event, int64, error) {
	if err := requireLocalTenant(tenantID); err != nil {
		return nil, 0, err
	}
	exists, err := r.Exists(ctx, tenantID, sessionID)
	if err != nil {
		return nil, 0, err
	}
	if !exists {
		return nil, 0, ErrRuntimeNotFound
	}
	journal, err := OpenRuntimeJournal(ctx, r.store.dir, sessionID)
	if err != nil {
		return nil, 0, err
	}
	defer journal.Close()
	events, err := journal.Load(ctx, afterSeq, limit)
	if err != nil {
		return nil, 0, err
	}
	head, err := journal.Head(ctx)
	return events, head, err
}

func (r *LocalRepository) ListSessions(_ context.Context, tenantID string) ([]Meta, error) {
	if err := requireLocalTenant(tenantID); err != nil {
		return nil, err
	}
	return r.store.ListSessions(), nil
}

func (r *LocalRepository) Pin(_ context.Context, tenantID, sessionID string, pinned bool) (*Meta, error) {
	if err := requireLocalTenant(tenantID); err != nil {
		return nil, err
	}
	return r.store.Pin(sessionID, pinned)
}

func (r *LocalRepository) UpdateMetaProjection(_ context.Context, tenantID, sessionID string,
	_ int64, messageCount, turnCount int) error {
	if err := requireLocalTenant(tenantID); err != nil {
		return err
	}
	return r.store.UpdateMetaProjection(sessionID, messageCount, turnCount)
}

func (*LocalRepository) Close() error { return nil }

func (*LocalRepository) Health(context.Context) error { return nil }

func (*LocalRepository) ClaimOutbox(context.Context, string, int, time.Duration) ([]OutboxRecord, error) {
	return nil, nil
}

func (*LocalRepository) MarkOutboxPublished(context.Context, string, []int64) error { return nil }

func requireLocalTenant(tenantID string) error {
	if tenantID = strings.TrimSpace(tenantID); tenantID != "" && tenantID != LocalTenantID {
		return errors.New("local session repository only supports tenant local")
	}
	return nil
}
