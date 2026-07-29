package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// These two signed int32 keys spell "LLMG" / "SRV1". The two-key advisory
	// lock namespace is stable across processes and independent of row IDs.
	serveLockNamespace int32 = 0x4c4c4d47
	serveLockIdentity  int32 = 0x53525631

	serveLockCloseTimeout = 5 * time.Second
)

// ErrServeLockHeld reports another active LLMGW serve process for this database.
var ErrServeLockHeld = errors.New("another LLMGW serve instance is active for this PostgreSQL database")

// ServeLock owns a PostgreSQL session connection for the complete serve lifetime.
type ServeLock struct {
	connection *pgxpool.Conn
	once       sync.Once
	err        error
}

// AcquireServeLock reserves the one supported serve process before recovery.
func (s *Store) AcquireServeLock(ctx context.Context) (*ServeLock, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("acquire LLMGW serve singleton PostgreSQL lock:\nstore is required")
	}
	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire LLMGW serve singleton PostgreSQL session:\n%w", err)
	}

	var acquired bool
	err = connection.QueryRow(
		ctx,
		`SELECT pg_try_advisory_lock($1, $2)`,
		serveLockNamespace,
		serveLockIdentity,
	).Scan(&acquired)
	if err != nil {
		connection.Release()
		return nil, fmt.Errorf("acquire LLMGW serve singleton PostgreSQL lock:\n%w", err)
	}
	if !acquired {
		connection.Release()
		return nil, fmt.Errorf(
			"acquire LLMGW serve singleton PostgreSQL lock:\n%w",
			ErrServeLockHeld,
		)
	}
	return &ServeLock{connection: connection}, nil
}

// Release unlocks the dedicated PostgreSQL session exactly once.
func (l *ServeLock) Release(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		var unlocked bool
		unlockErr := l.connection.QueryRow(
			ctx,
			`SELECT pg_advisory_unlock($1, $2)`,
			serveLockNamespace,
			serveLockIdentity,
		).Scan(&unlocked)
		if unlockErr == nil && unlocked {
			l.connection.Release()
			return
		}

		closeCtx, cancel := context.WithTimeout(context.Background(), serveLockCloseTimeout)
		closeErr := l.connection.Conn().Close(closeCtx)
		cancel()
		l.connection.Release()
		if unlockErr != nil {
			l.err = fmt.Errorf(
				"release LLMGW serve singleton PostgreSQL lock:\n%w",
				errors.Join(unlockErr, closeErr),
			)
			return
		}
		l.err = fmt.Errorf(
			"release LLMGW serve singleton PostgreSQL lock:\n%w",
			errors.Join(errors.New("session did not own the advisory lock"), closeErr),
		)
	})
	return l.err
}
