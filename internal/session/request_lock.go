package session

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Send locks protect the entire keyed send, including its durable intent and
// outcome update. Unlike an HTTP-handler mutex they coordinate independent
// repository handles/processes. A process crash releases the OS/DB lock; the
// transcript, not a reservation or an in-memory cache, is the replay authority.
func sendLockHash(sessionID, key string) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%d:%s%s", len(sessionID), sessionID, key)))
}

func lockWait(ctx context.Context, try func() (bool, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := try()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func lockSendFile(ctx context.Context, prefix, sessionID, key string) (func(), error) {
	hash := sendLockHash(sessionID, key)
	f, err := os.OpenFile(prefix+".send-"+hex.EncodeToString(hash[:])+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("session send lock: %w", err)
	}
	if err := lockWait(ctx, func() (bool, error) { return trySendFileLock(f) }); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("session send lock: %w", err)
	}
	// Never unlink: a waiter may already hold this inode open. Closing releases
	// the advisory lock even on process death; the empty file is not a claim.
	return func() { _ = f.Close() }, nil
}

func (s *JSONLStore) LockMessage(ctx context.Context, sessionID, key string) (func(), error) {
	return lockSendFile(ctx, filepath.Join(s.root, "request"), sessionID, key)
}

var memorySendLocks sync.Map // *sql.DB -> *sync.Map of session/key -> semaphore

func (s *SQLStore) LockMessage(ctx context.Context, sessionID, key string) (func(), error) {
	if s.dialect.Name() == BackendPostgres {
		// Take configuration briefly, then release the data connection BEFORE
		// waiting. Lock waiters must not exhaust the pool the winner writes to.
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return nil, err
		}
		var cfg *pgx.ConnConfig
		err = conn.Raw(func(driverConn any) error {
			c, ok := driverConn.(*stdlib.Conn)
			if !ok {
				return fmt.Errorf("session send lock: not a pgx connection")
			}
			cfg = c.Conn().Config().Copy()
			return nil
		})
		_ = conn.Close()
		if err != nil {
			return nil, err
		}
		return lockPostgresSend(ctx, cfg, sessionID, key)
	}
	// SQLite serialises individual statements, not a read/append/fanout/update
	// sequence. Lock the database's canonical file, without holding its sole
	// connection or a transaction across network delivery.
	rows, err := s.db.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return nil, err
	}
	var file string
	for rows.Next() {
		var seq int
		var name, path string
		if err := rows.Scan(&seq, &name, &path); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if name == "main" {
			file = path
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if file != "" {
		return lockSendFile(ctx, file, sessionID, key)
	}
	// :memory: is private to this DB handle. There is no cross-process file.
	value, _ := memorySendLocks.LoadOrStore(s.db, &sync.Map{})
	locks := value.(*sync.Map)
	hash := sendLockHash(sessionID, key)
	value, _ = locks.LoadOrStore(hash, make(chan struct{}, 1))
	sem := value.(chan struct{})
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *PostgresStore) LockMessage(ctx context.Context, sessionID, key string) (func(), error) {
	return lockPostgresSend(ctx, s.pool.Config().ConnConfig.Copy(), sessionID, key)
}

func lockPostgresSend(ctx context.Context, cfg *pgx.ConnConfig, sessionID, key string) (func(), error) {
	// A dedicated connection owns the advisory lock, not a pooled data slot.
	// Closing it releases the lock on success, cancellation or process death.
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	closeConn := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}
	hash := sendLockHash(sessionID, key)
	id := int64(binary.BigEndian.Uint64(hash[:8]))
	if err := lockWait(ctx, func() (bool, error) {
		var ok bool
		err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", id).Scan(&ok)
		return ok, err
	}); err != nil {
		closeConn()
		return nil, err
	}
	return closeConn, nil
}
