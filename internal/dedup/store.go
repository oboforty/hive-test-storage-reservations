// Package dedup implements request idempotency on top of the dedup table.
//
// It is model-agnostic: the Middleware claims a client-supplied key before
// the wrapped handler runs, stores the handler's successful response, and
// replays that response for any later request with the same key.
package dedup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrMissingKey: the request has no dedup key but one is required.
	ErrMissingKey = errors.New("dedup key is required")
	// ErrInvalidKey: the key is empty or too long.
	ErrInvalidKey = errors.New("invalid dedup key")
	// ErrInProgress: another request with the same key is still being processed.
	ErrInProgress = errors.New("a request with this dedup key is still in progress")
	// ErrKeyReused: the key was already used for a different request.
	ErrKeyReused = errors.New("dedup key was already used for a different request")
)

// Entry mirrors a row of the dedup table.
type Entry struct {
	Key                string
	RequestHash        string
	Model              *string
	ModelID            *string
	ResponseMethod     *string
	ResponseBody       *string
	ResponseStatusCode *int
	CreatedAt          time.Time
	ExpiresAt          *time.Time
	CompletedAt        *time.Time
}

// Pending reports whether the request owning this entry has not finished yet.
func (e Entry) Pending() bool { return e.CompletedAt == nil }

// Store reads and writes dedup entries.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore creates a Store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// Claim atomically reserves key for the current request.
//
// It returns (nil, nil) when the key is new (or its previous entry expired):
// the caller now owns the key and must later call Complete or Release.
// Otherwise it returns the existing entry, untouched.
//
// The lookup and insert run in one transaction; with the database's
// BEGIN IMMEDIATE locking this serializes concurrent claims for the same key.
func (s *Store) Claim(ctx context.Context, key, requestHash, model, method string, ttl time.Duration) (*Entry, error) {
	var existing *Entry
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		e, err := get(ctx, tx, key)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := s.now()
		if err == nil {
			if e.ExpiresAt == nil || now.Before(*e.ExpiresAt) {
				existing = &e
				return nil
			}
			// Expired: forget it and treat the key as new.
			if _, err := tx.ExecContext(ctx, `DELETE FROM dedup WHERE key = ?`, key); err != nil {
				return fmt.Errorf("delete expired dedup entry: %w", err)
			}
		}

		var expiresAt *time.Time
		if ttl > 0 {
			t := now.Add(ttl)
			expiresAt = &t
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO dedup (key, request_hash, model, response_method, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			key, requestHash, nullIfEmpty(model), method, now, expiresAt)
		if err != nil {
			return fmt.Errorf("insert dedup entry: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return existing, nil
}

// Complete stores the response of the request that owns key and marks it done.
func (s *Store) Complete(ctx context.Context, key string, status int, body []byte, modelID string) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE dedup
		SET response_status_code = ?, response_body = ?, model_id = COALESCE(?, model_id), completed_at = ?
		WHERE key = ?`,
		status, string(body), nullIfEmpty(modelID), now, key)
	if err != nil {
		return fmt.Errorf("complete dedup entry %s: %w", key, err)
	}
	return nil
}

// Release deletes a pending claim so the key can be retried. Completed
// entries are never released.
func (s *Store) Release(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dedup WHERE key = ? AND completed_at IS NULL`, key)
	if err != nil {
		return fmt.Errorf("release dedup entry %s: %w", key, err)
	}
	return nil
}

// Get returns the entry for key, or sql.ErrNoRows.
func (s *Store) Get(ctx context.Context, key string) (Entry, error) {
	return get(ctx, s.db, key)
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func get(ctx context.Context, q queryRower, key string) (Entry, error) {
	var (
		e                            Entry
		model, modelID, method, body sql.NullString
		status                       sql.NullInt64
		expiresAt, completedAt       sql.NullTime
	)
	err := q.QueryRowContext(ctx, `
		SELECT key, request_hash, model, model_id, response_method, response_body,
		       response_status_code, created_at, expires_at, completed_at
		FROM dedup WHERE key = ?`, key).
		Scan(&e.Key, &e.RequestHash, &model, &modelID, &method, &body,
			&status, &e.CreatedAt, &expiresAt, &completedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, err
		}
		return Entry{}, fmt.Errorf("get dedup entry: %w", err)
	}
	e.Model = strPtr(model)
	e.ModelID = strPtr(modelID)
	e.ResponseMethod = strPtr(method)
	e.ResponseBody = strPtr(body)
	if status.Valid {
		v := int(status.Int64)
		e.ResponseStatusCode = &v
	}
	e.ExpiresAt = timePtr(expiresAt)
	e.CompletedAt = timePtr(completedAt)
	return e, nil
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}
