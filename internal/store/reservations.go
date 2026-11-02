package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Reservation mirrors the storage_reservations table.
type Reservation struct {
	ID          string     `json:"id"`
	AccountID   string     `json:"account_id"`
	Size        int64      `json:"size"`
	DedupID     *string    `json:"dedup_id"`
	State       string     `json:"state"`
	DebugLog    *string    `json:"debug_log"`
	ErrorLog    *string    `json:"error_log"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   *time.Time `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

const reservationColumns = `id, account_id, size, dedup_id, state, debug_log, error_log,
	created_at, updated_at, deleted_at, completed_at`

func scanReservation(row interface{ Scan(...any) error }) (Reservation, error) {
	var (
		r                                 Reservation
		dedupID, debugLog, errorLog       sql.NullString
		updatedAt, deletedAt, completedAt sql.NullTime
	)
	if err := row.Scan(&r.ID, &r.AccountID, &r.Size, &dedupID, &r.State, &debugLog, &errorLog,
		&r.CreatedAt, &updatedAt, &deletedAt, &completedAt); err != nil {
		return Reservation{}, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	r.DedupID = nullStringPtr(dedupID)
	r.DebugLog = nullStringPtr(debugLog)
	r.ErrorLog = nullStringPtr(errorLog)
	r.UpdatedAt = nullTimePtr(updatedAt)
	r.DeletedAt = nullTimePtr(deletedAt)
	r.CompletedAt = nullTimePtr(completedAt)
	return r, nil
}

func (s *Store) queryReservations(ctx context.Context, q querier, where string, args ...any) ([]Reservation, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+reservationColumns+` FROM storage_reservations WHERE `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list reservations: %w", err)
	}
	defer rows.Close()

	out := []Reservation{}
	for rows.Next() {
		r, err := scanReservation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan reservation: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func deletedFilter(includeDeleted bool) string {
	if includeDeleted {
		return "1=1"
	}
	return "deleted_at IS NULL"
}

// ListReservations returns reservations across all accounts.
func (s *Store) ListReservations(ctx context.Context, includeDeleted bool) ([]Reservation, error) {
	return s.queryReservations(ctx, s.db, deletedFilter(includeDeleted))
}

// ListAccountReservations returns reservations of a single account.
func (s *Store) ListAccountReservations(ctx context.Context, accountID string, includeDeleted bool) ([]Reservation, error) {
	return s.queryReservations(ctx, s.db, `account_id = ? AND `+deletedFilter(includeDeleted), accountID)
}

// GetReservation returns a reservation that belongs to the given account.
// Soft-deleted reservations are still returned (with deleted_at set).
func (s *Store) GetReservation(ctx context.Context, accountID, reservationID string) (Reservation, error) {
	return getReservation(ctx, s.db, accountID, reservationID)
}

func getReservation(ctx context.Context, q querier, accountID, reservationID string) (Reservation, error) {
	r, err := scanReservation(q.QueryRowContext(ctx,
		`SELECT `+reservationColumns+` FROM storage_reservations WHERE id = ? AND account_id = ?`,
		reservationID, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, fmt.Errorf("reservation %s: %w", reservationID, ErrNotFound)
	}
	if err != nil {
		return Reservation{}, fmt.Errorf("get reservation: %w", err)
	}
	return r, nil
}

func insertHistory(ctx context.Context, q querier, r Reservation, op, lastState string, at time.Time) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO storage_history (reservation_id, operation, account_id, size, last_state, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, op, r.AccountID, r.Size, lastState, at)
	if err != nil {
		return fmt.Errorf("insert history: %w", err)
	}
	return nil
}

// CreateReservation reserves size bytes for an account.
//
// Step 1 (one IMMEDIATE transaction): verify the account has fewer than
// max_parallel_chunks active reservations and enough free space, insert the
// reservation in state "pending" and write a "reserve" history entry.
// Step 2: mark the reservation "completed" (sets completed_at/updated_at).
//
// dedupID (optional) links the reservation to the dedup entry of the request
// that created it.
func (s *Store) CreateReservation(ctx context.Context, accountID string, size int64, debugLog, dedupID *string) (Reservation, error) {
	if size <= 0 {
		return Reservation{}, fmt.Errorf("size must be a positive number of bytes: %w", ErrInvalidInput)
	}

	now := s.now()
	r := Reservation{
		ID:        uuid.NewString(),
		AccountID: accountID,
		Size:      size,
		DedupID:   dedupID,
		State:     StatePending,
		DebugLog:  debugLog,
		CreatedAt: now,
	}

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		acc, err := getAccount(ctx, tx, `id = ?`, accountID)
		if err != nil {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM storage_reservations WHERE account_id = ? AND deleted_at IS NULL`,
			accountID).Scan(&active); err != nil {
			return fmt.Errorf("count reservations: %w", err)
		}
		if active >= acc.MaxParallelChunks {
			return &TooManyReservationsError{AccountID: accountID, Limit: acc.MaxParallelChunks, Active: active}
		}

		reserved, err := reservedStorage(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if free := acc.MaxStorage - reserved; size > free {
			return &InsufficientStorageError{AccountID: accountID, Requested: size, Free: max(free, 0)}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO storage_reservations (id, account_id, size, dedup_id, state, debug_log, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.AccountID, r.Size, r.DedupID, r.State, r.DebugLog, r.CreatedAt); err != nil {
			return fmt.Errorf("insert reservation: %w", err)
		}
		return insertHistory(ctx, tx, r, OpReserve, r.State, now)
	})
	if err != nil {
		return Reservation{}, err
	}

	return s.completeReservation(ctx, r)
}

func (s *Store) completeReservation(ctx context.Context, r Reservation) (Reservation, error) {
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE storage_reservations SET state = ?, completed_at = ?, updated_at = ?
		WHERE id = ?`, StateCompleted, now, now, r.ID); err != nil {
		return Reservation{}, fmt.Errorf("complete reservation %s: %w", r.ID, err)
	}
	return s.GetReservation(ctx, r.AccountID, r.ID)
}

// SoftDeleteReservation releases a single reservation by setting deleted_at.
func (s *Store) SoftDeleteReservation(ctx context.Context, accountID, reservationID string) (Reservation, error) {
	var out Reservation
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		r, err := getReservation(ctx, tx, accountID, reservationID)
		if err != nil {
			return err
		}
		if r.DeletedAt != nil {
			return fmt.Errorf("reservation %s already deleted: %w", reservationID, ErrNotFound)
		}
		if err := s.softDelete(ctx, tx, r); err != nil {
			return err
		}
		out, err = getReservation(ctx, tx, accountID, reservationID)
		return err
	})
	return out, err
}

// SoftDeleteAccountReservations releases every active reservation of an account.
func (s *Store) SoftDeleteAccountReservations(ctx context.Context, accountID string) (int, error) {
	var n int
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := getAccount(ctx, tx, `id = ?`, accountID); err != nil {
			return err
		}
		active, err := s.queryReservations(ctx, tx, `account_id = ? AND deleted_at IS NULL`, accountID)
		if err != nil {
			return err
		}
		for _, r := range active {
			if err := s.softDelete(ctx, tx, r); err != nil {
				return err
			}
		}
		n = len(active)
		return nil
	})
	return n, err
}

func (s *Store) softDelete(ctx context.Context, tx *sql.Tx, r Reservation) error {
	now := s.now()
	if _, err := tx.ExecContext(ctx, `
		UPDATE storage_reservations SET state = ?, deleted_at = ?, updated_at = ?
		WHERE id = ?`, StateReleased, now, now, r.ID); err != nil {
		return fmt.Errorf("soft delete reservation %s: %w", r.ID, err)
	}
	return insertHistory(ctx, tx, r, OpRelease, r.State, now)
}

// ClearAccountReservations permanently deletes all reservations of an account
// (including soft-deleted ones). Active reservations get a "release" history
// entry first, so storage_history stays a complete audit trail.
func (s *Store) ClearAccountReservations(ctx context.Context, accountID string) (int, error) {
	var n int
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		active, err := s.queryReservations(ctx, tx, `account_id = ? AND deleted_at IS NULL`, accountID)
		if err != nil {
			return err
		}
		now := s.now()
		for _, r := range active {
			if err := insertHistory(ctx, tx, r, OpRelease, r.State, now); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM storage_reservations WHERE account_id = ?`, accountID)
		if err != nil {
			return fmt.Errorf("delete reservations: %w", err)
		}
		affected, err := res.RowsAffected()
		n = int(affected)
		return err
	})
	return n, err
}
