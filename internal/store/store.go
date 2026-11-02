// Package store contains the data access and storage-allocation logic.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotFound is returned when an account or reservation does not exist
	// (or is soft-deleted).
	ErrNotFound = errors.New("not found")
	// ErrInvalidInput is returned for semantically invalid input.
	ErrInvalidInput = errors.New("invalid input")
)

// InsufficientStorageError is returned when a reservation does not fit into
// the account's free storage.
type InsufficientStorageError struct {
	AccountID string
	Requested int64
	Free      int64
}

func (e *InsufficientStorageError) Error() string {
	return fmt.Sprintf("insufficient storage: requested %d bytes, %d bytes free", e.Requested, e.Free)
}

// TooManyReservationsError is returned when an account already has
// max_parallel_chunks active reservations.
type TooManyReservationsError struct {
	AccountID string
	Limit     int
	Active    int
}

func (e *TooManyReservationsError) Error() string {
	return fmt.Sprintf("too many active reservations: %d of max %d", e.Active, e.Limit)
}

// Reservation states.
const (
	StatePending   = "pending"
	StateCompleted = "completed"
	StateReleased  = "released"
)

// History operations.
const (
	OpReserve = "reserve"
	OpRelease = "release"
)

// Store wraps the database handle.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// New creates a Store.
func New(db *sql.DB) *Store {
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// DB exposes the underlying handle (used for health checks).
func (s *Store) DB() *sql.DB { return s.db }

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func nullStringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}
