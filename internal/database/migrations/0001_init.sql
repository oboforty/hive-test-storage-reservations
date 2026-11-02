CREATE TABLE accounts (
    id                  TEXT     PRIMARY KEY,
    username            TEXT     NOT NULL UNIQUE,
    max_storage         INTEGER  NOT NULL CHECK (max_storage >= 0),       -- bytes
    max_parallel_chunks INTEGER  NOT NULL DEFAULT 4 CHECK (max_parallel_chunks > 0),
    created_at          DATETIME NOT NULL,
    updated_at          DATETIME NULL,
    deleted_at          DATETIME NULL
);

CREATE TABLE storage_reservations (
    id           TEXT     PRIMARY KEY,
    account_id   TEXT     NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    size         INTEGER  NOT NULL CHECK (size > 0),
    -- logical ref to dedup.key
    dedup_id     TEXT     NULL,                         
    state        TEXT     NOT NULL,
    debug_log    TEXT     NULL,
    error_log    TEXT     NULL,
    created_at   DATETIME NOT NULL,
    updated_at   DATETIME NULL,
    deleted_at   DATETIME NULL,
    completed_at DATETIME NULL
);

CREATE INDEX idx_reservations_account_active
    ON storage_reservations (account_id) WHERE deleted_at IS NULL;


-- event log so that every reservation/release can be tracked back
CREATE TABLE storage_history (
    id             INTEGER  PRIMARY KEY AUTOINCREMENT,
    reservation_id TEXT     NOT NULL,
    operation      TEXT     NOT NULL CHECK (operation IN ('reserve', 'release')),
    account_id     TEXT     NOT NULL,
    size           INTEGER  NOT NULL,
    last_state     TEXT     NOT NULL,
    created_at     DATETIME NOT NULL
);

CREATE INDEX idx_history_account ON storage_history (account_id);
CREATE INDEX idx_history_reservation ON storage_history (reservation_id);

-- idempotency table, to reject request duplicates
CREATE TABLE dedup (
    key                  TEXT     PRIMARY KEY,
    request_hash         TEXT     NOT NULL UNIQUE,
    model                TEXT     NULL,
    model_id             TEXT     NULL,
    response_method      TEXT     NULL,
    response_body        TEXT     NULL,
    response_status_code INTEGER  NULL,
    created_at           DATETIME NOT NULL,
    expires_at           DATETIME NULL
);
