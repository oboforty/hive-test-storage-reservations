package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Account mirrors the accounts table.
type Account struct {
	ID                string     `json:"id"`
	Username          string     `json:"username"`
	MaxStorage        int64      `json:"max_storage"`
	MaxParallelChunks int        `json:"max_parallel_chunks"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         *time.Time `json:"updated_at"`
	DeletedAt         *time.Time `json:"deleted_at"`
}

// AccountSummary is an account plus derived storage usage information.
type AccountSummary struct {
	Account
	ReservedStorage int64   `json:"reserved_storage"`
	FreeStorage     int64   `json:"free_storage"`
	Usage           float64 `json:"used_percent"`
}

// AccountDetails is an account summary plus its active reservations.
type AccountDetails struct {
	AccountSummary
	ActiveReservations []Reservation `json:"active_reservations"`
}

const accountColumns = `id, username, max_storage, max_parallel_chunks, created_at, updated_at, deleted_at`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var (
		a                    Account
		updatedAt, deletedAt sql.NullTime
	)
	if err := row.Scan(&a.ID, &a.Username, &a.MaxStorage, &a.MaxParallelChunks,
		&a.CreatedAt, &updatedAt, &deletedAt); err != nil {
		return Account{}, err
	}
	a.CreatedAt = a.CreatedAt.UTC()
	a.UpdatedAt = nullTimePtr(updatedAt)
	a.DeletedAt = nullTimePtr(deletedAt)
	return a, nil
}

// ListAccounts returns all non-deleted accounts ordered by creation time.
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE deleted_at IS NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount returns a non-deleted account by id.
func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	return getAccount(ctx, s.db, `id = ?`, id)
}

// FindAccount resolves an account by id or, failing that, by username.
func (s *Store) FindAccount(ctx context.Context, idOrUsername string) (Account, error) {
	a, err := getAccount(ctx, s.db, `id = ?`, idOrUsername)
	if errors.Is(err, ErrNotFound) {
		return getAccount(ctx, s.db, `username = ?`, idOrUsername)
	}
	return a, err
}

func getAccount(ctx context.Context, q querier, where string, arg any) (Account, error) {
	a, err := scanAccount(q.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE `+where+` AND deleted_at IS NULL`, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("account %v: %w", arg, ErrNotFound)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	return a, nil
}

// reservedStorage sums the size of all non-deleted reservations of an account.
func reservedStorage(ctx context.Context, q querier, accountID string) (int64, error) {
	var sum int64
	err := q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size), 0) FROM storage_reservations WHERE account_id = ? AND deleted_at IS NULL`,
		accountID).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("sum reservations: %w", err)
	}
	return sum, nil
}

// AccountSummary computes reserved/free storage and usage for an account.
func (s *Store) AccountSummary(ctx context.Context, a Account) (AccountSummary, error) {
	reserved, err := reservedStorage(ctx, s.db, a.ID)
	if err != nil {
		return AccountSummary{}, err
	}

	var pct float64
	if a.MaxStorage > 0 {
		pct = float64(reserved) / float64(a.MaxStorage)
	}
	return AccountSummary{
		Account:         a,
		ReservedStorage: reserved,
		FreeStorage:     max(a.MaxStorage-reserved, 0),
		Usage:           pct,
	}, nil
}

// AccountDetails is AccountSummary plus the list of active (non-deleted)
// reservations of the account.
func (s *Store) AccountDetails(ctx context.Context, a Account) (AccountDetails, error) {
	summary, err := s.AccountSummary(ctx, a)
	if err != nil {
		return AccountDetails{}, err
	}
	active, err := s.ListAccountReservations(ctx, a.ID, false)
	if err != nil {
		return AccountDetails{}, err
	}
	return AccountDetails{AccountSummary: summary, ActiveReservations: active}, nil
}
