# Hive storage reserver

A Go + [chi](https://github.com/go-chi/chi) HTTP service that does **logical** storage
allocation per account, backed by SQLite (`modernc.org/sqlite`, pure Go, no CGO).

All sizes are in **bytes**. The seeded `hive` account gets 100 GiB (`100 * 1024^3`).

## Quick start

```sh
cp .env.example .env          # already done once; edit as needed
go run . migrate              # create the schema
go run . seed                 # account "hive" (100 GiB) + placeholder dedup row key=1
go run .                      # serve on :8080   (same as: go run . serve)
go run . serve -seed          # migrate + seed + serve in one go
```

Docker:

```sh
docker compose up --build     # runs `serve -seed`; data and logs live in named volumes
```

## CLI commands

| Command | Description |
|---|---|
| `serve [-migrate] [-seed]` | Run the HTTP API (default). `-seed` implies `-migrate`. |
| `migrate` | Apply pending migrations (`internal/database/migrations/*.sql`, tracked in `schema_migrations`). |
| `seed` | Insert seed data. Safe to run more than once. |
| `account <id\|username>` | Print account data as JSON: table fields plus `reserved_storage`, `free_storage`, `used_percent`, `active_reservations`. |
| `clear-reservations <id\|username>` | **Permanently** delete every reservation of the account (a `release` history row is written for each active one first). |

## HTTP API

| Method | Path | Description |
|---|---|---|
| GET | `/accounts[?full=1]` | List accounts. `full=1` adds `reserved_storage`, `free_storage`, `used_percent` (no reservation list). |
| GET | `/accounts/{id}[?full=1]` | One account. `full=1` adds the same fields as the `account` CLI command, including `active_reservations`. |
| GET | `/reservations[?include_deleted=1]` | All reservations (soft-deleted ones only with `include_deleted=1`). |
| GET | `/accounts/{id}/reservations[?include_deleted=1]` | Reservations of one account. |
| POST | `/accounts/{id}/reservations` | Requires `X-Dedup-ID`. Body `{"size": <bytes>, "debug_log": "optional"}`. Returns `201` with the reservation (incl. generated `id`), or `409` if there is not enough free space or the account already has `max_parallel_chunks` active reservations. |
| DELETE | `/accounts/{id}/reservations` | Soft-delete **all** active reservations of the account. |
| GET | `/accounts/{id}/reservations/{rid}` | One reservation. |
| DELETE | `/accounts/{id}/reservations/{rid}` | Soft-delete one reservation (`deleted_at` = now UTC, `state` = `released`). |
| GET | `/health` | DB ping. |

Reservation lifecycle: `pending` (inserted after the free-space check, in one
`BEGIN IMMEDIATE` transaction, so concurrent requests can't oversubscribe an account)
→ `completed` (sets `completed_at` / `updated_at`) → `released` (soft delete).
Every reserve/release is recorded in `storage_history`.

## Idempotency (`X-Dedup-ID`)

`POST /accounts/{id}/reservations` and both `DELETE` reservation endpoints require an
`X-Dedup-ID` header (any client-generated string up to 255 chars, e.g. a UUID). The
generic middleware in `internal/dedup` handles it, independent of the reservation code:

| State of the `dedup` row for the key | Result |
|---|---|
| none (or expired, see `DEDUP_TTL`) | Request runs. A 2xx response is stored and `completed_at` set; any other outcome deletes the row so the key can be retried. |
| `completed_at` set | Stored status code and body are replayed (`X-Dedup-Replayed: true`). |
| `completed_at` NULL (in flight) | `409` |
| created by a different method/URI/body | `422` |
| header missing | `400` |

Created reservations store the key in `dedup_id` (foreign key to `dedup.key`); the dedup
row's `model`/`model_id` point back at the reservation. To use it for another model:
`r.With(dedupStore.Middleware(dedup.Options{Model: "thing", TTL: ...}))` and optionally
`dedup.SetModelID(ctx, id)` / `dedup.KeyFromContext(ctx)` in the handler.

Free storage = `max_storage - SUM(size)` of all non-deleted reservations.

## Reservation count limit (`max_parallel_chunks`)

An account can hold at most `max_parallel_chunks` active (non-deleted) reservations;
further `POST`s get `409 too many active reservations`. The value for **new** accounts
(currently: the seeded `hive` account) comes from `settings.GetDefaultMaxParallelChunks`:

1. `hivesettings.json` (`HIVE_SETTINGS_FILE`): `{"default_max_parallel_chunks": 8}`
2. if missing/invalid: `GET $HIVE_API`, expecting the same JSON; a valid answer is cached
   into `hivesettings.json`, so the API is only called once
3. if `HIVE_API` is unset or fails (network error, 404/any non-2xx, bad JSON, value <= 0):
   `ACCOUNT_MAX_CHUNKS_DEFAULT` (default `5`)

Delete `hivesettings.json` to force a refresh from the API.

## Configuration (`.env`)

`.env` is loaded first (it does not override variables already set in the process
environment), then `.env.override` (overrides everything).

| Variable | Default | |
|---|---|---|
| `ENV` | `DEBUG` | `DEBUG`, `TESTING` or `PROD` |
| `HTTP_ADDR` | `:8080` | |
| `DB_PATH` | `data/hive.db` | |
| `LOG_FILE` | `logs/app.log` | JSON lines |
| `SHUTDOWN_TIMEOUT` | `15s` | Graceful drain time on SIGINT/SIGTERM |
| `REQUEST_TIMEOUT` | `30s` | Context deadline per request |
| `DEDUP_TTL` | `24h` | How long dedup entries are replayed; `0` = forever |
| `HIVE_SETTINGS_FILE` | `hivesettings.json` | Local settings / cache of `HIVE_API` |
| `HIVE_API` | _(empty)_ | URL returning `{"default_max_parallel_chunks": N}` |
| `ACCOUNT_MAX_CHUNKS_DEFAULT` | `5` | Last-resort default chunk limit |
| `RATE_LIMIT` | `10` | Requests per second per client IP; `0` disables. Excess requests get `429` with `Retry-After` |
| `TRUSTED_PROXIES` | _(empty)_ | Comma-separated CIDRs whose `X-Forwarded-For` is trusted for the client IP; empty = TCP peer address |

Logging (below WARN goes to stdout, WARN and above to stderr):

| ENV | log file | stdout / stderr |
|---|---|---|
| DEBUG | debug+ | debug+ |
| TESTING | debug+ | info+ |
| PROD | info+ | errors only |

Errors: handlers return errors to a global middleware, which also recovers panics.
With `ENV=PROD`, 5xx responses are just `{"error": "Internal Server Error"}` (4xx keep
their message, e.g. `insufficient storage`). In other modes the body includes the error
chain, type, request id, method, path, query, timestamp and, for panics, the stack trace.

## Tests

```sh
go test ./...
```
