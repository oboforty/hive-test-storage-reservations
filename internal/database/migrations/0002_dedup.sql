-- Idempotency: a dedup entry is "pending" while completed_at IS NULL.
ALTER TABLE dedup ADD COLUMN completed_at DATETIME NULL;

-- Mark pre-existing entries (e.g. the seed placeholder) as completed so they
-- are not treated as in-flight requests.
UPDATE dedup SET completed_at = created_at WHERE completed_at IS NULL;

-- SQLite cannot add a foreign key to an existing table: rebuild
-- storage_reservations with dedup_id -> dedup(key).
CREATE TABLE storage_reservations_new (
    id           TEXT     PRIMARY KEY,
    account_id   TEXT     NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    size         INTEGER  NOT NULL CHECK (size > 0),
    dedup_id     TEXT     NULL REFERENCES dedup(key) ON DELETE SET NULL,
    state        TEXT     NOT NULL,
    debug_log    TEXT     NULL,
    error_log    TEXT     NULL,
    created_at   DATETIME NOT NULL,
    updated_at   DATETIME NULL,
    deleted_at   DATETIME NULL,
    completed_at DATETIME NULL
);

INSERT INTO storage_reservations_new
    (id, account_id, size, dedup_id, state, debug_log, error_log,
     created_at, updated_at, deleted_at, completed_at)
SELECT id, account_id, size,
       CASE WHEN dedup_id IN (SELECT key FROM dedup) THEN dedup_id END,
       state, debug_log, error_log, created_at, updated_at, deleted_at, completed_at
FROM storage_reservations;

DROP TABLE storage_reservations;
ALTER TABLE storage_reservations_new RENAME TO storage_reservations;

CREATE INDEX idx_reservations_account_active
    ON storage_reservations (account_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_reservations_dedup ON storage_reservations (dedup_id);
