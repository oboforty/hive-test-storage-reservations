package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

const (
	GB = int64(1) << 30

	SeedUsername   = "hive"
	SeedMaxStorage = 100 * GB
	SeedDedupKey   = "1"
	seedDedupHash  = "seed-placeholder"
)

// Seed inserts the initial data. It is idempotent: running it twice does not
// create duplicates. maxParallelChunks is used for newly created accounts
// (see settings.GetDefaultMaxParallelChunks); existing accounts are untouched.
func Seed(ctx context.Context, db *sql.DB, log *slog.Logger, maxParallelChunks int) error {
	now := time.Now().UTC()
	return inTx(ctx, db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO accounts (id, username, max_storage, max_parallel_chunks, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (username) DO NOTHING`,
			uuid.NewString(), SeedUsername, SeedMaxStorage, maxParallelChunks, now)
		if err != nil {
			return fmt.Errorf("seed account: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.InfoContext(ctx, "seeded account", "username", SeedUsername,
				"max_storage", SeedMaxStorage, "max_parallel_chunks", maxParallelChunks)
		}

		// @TODO: TEMPORAL CODE --
		res, err = tx.ExecContext(ctx, `
			INSERT INTO dedup (key, request_hash, created_at, completed_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (key) DO NOTHING`,
			SeedDedupKey, seedDedupHash, now, now)
		if err != nil {
			return fmt.Errorf("seed dedup: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.InfoContext(ctx, "seeded dedup placeholder", "key", SeedDedupKey)
		}
		return nil
	})
}
