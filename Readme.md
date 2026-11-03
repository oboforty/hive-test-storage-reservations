# Hivenet storage reservations API

This is a [chi](https://github.com/go-chi/chi) HTTP service, which handles reservation of logical storage space for Hivenet accounts. 


## Quick Start

Building the compose file is sufficient, as it will run Database migrations and seeding:

    docker compose up --build -d

The service can be restarted, as :

    docker compose restart
   
Please mind the persistent `hive-data` and `hive-logs` volumes when deploying the service.

To clear service (deleting the data & logs volumes as well):

    docker compose down -v

The service is available on `127:0.0.1:8080`

**To check the container's logs:**

    docker run --rm -v hiveproj_hive-logs:/logs alpine sh -c 'cat /logs/*'

## API 
I originally wanted to add an OpenAPI endpoint, but ran out of time, so here are the endpoints. You can also see them in `./.bruno` as a [Bruno](https://www.usebruno.com/) collection. With Bruno, you can confirm the core endpoints, and API idempotence for reservations (deduplication keys), but not concurrency. For that either refer to the tests, or the python client script:
   
    python concurrency_client.py --calls 10

Endpoints:

| Method | Path | Description |
|---|---|---|
| GET | `/accounts[?full=1]` | List accounts. `full=1` adds `reserved_storage`, `free_storage`, `used_percent`. Free storage = `max_storage - SUM(size)` of all non-deleted reservations. |
| GET | `/accounts/{id}[?full=1]` | Gets account info. `full=1` adds the same fields as the `account` CLI command. |
| GET | `/reservations[?include_deleted=1]` | All reservations (soft-deleted ones only with `include_deleted=1`). |
| GET | `/accounts/{id}/reservations[?include_deleted=1]` | Reservations of one account. |
| POST | `/accounts/{id}/reservations` | Requires `X-Dedup-ID`. Body `{"size": <bytes to be reserved>}`. Returns `201` with the reservation (incl. generated `id`), or `409` if there is not enough free space or the account already has `max_parallel_chunks` active reservations. |
| DELETE | `/accounts/{id}/reservations` | Soft-deletes **all** active reservations of the account. |
| GET | `/accounts/{id}/reservations/{rid}` | One reservation. |
| DELETE | `/accounts/{id}/reservations/{rid}` | Soft-deletes one reservation (`deleted_at` = now UTC, `state` = `released`). |
| GET | `/health` | Health check. I originally wanted to include NOK as a response, in case of DB inconsistency (reservations vs account's MAX config), and in cases of unreachable HIVE_API.  |

### Reservation life cycle

`pending` (inserted after the free-space check, in one
`BEGIN IMMEDIATE` transaction, so concurrent requests can't oversubscribe an account)
→ `completed` (sets `completed_at` / `updated_at`) → `released` (soft delete).
Every reserve/release is recorded in `storage_history`.

Since request deduplication is stored in the same RelDB, Sqlite's ACIDity guarantees stateless execution. `POST /accounts/{id}/reservations` and both `DELETE` reservation endpoints require an `X-Dedup-ID` header (any client-generated string up to 255 chars, e.g. a UUID). The generic middleware in `internal/dedup` handles it, independent of the reservation code:

An account can hold at most `max_parallel_chunks` active reservations. This config is requested from an external service (`HIVE_API` envvar), and a default value of `5` is assumed. A local `hivesettings.json` file caches this value.


## Work spent on this demo

In total i spent ~3 hours on the development, documentation & testing of this service. I originally wanted to use duckdb and redlock, but decided to keep the service simple, so went with Sqlite for both data & request deduplication.

### AI usage
After designing the service, I wrote a specification including DB layout, required queries, API endpoints, middlewares, and constraints for reservations. I generated the trivial, boilerplate code (Claude Code), and manually corrected it, fixed wrong assumptions, and removed redundant tests, wrote new ones for untested core behaviour.

Then after making a decision on using deduplication headers, I wrote another specification to add the deduplication and rate throttling middlewares.

### Missing work

- 


----


## Local setup

The service self-contains all utility commands, so in lack of a docker, you can locall run:

    go run . migrate

Seeding creates a default account 'hive' with 100GB of free space:

    go run . seed

Running:

    go run . 


### Tests

    go test ./...


### Configuration (`.env`)

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
| `HIVE_API` | _(empty)_ | URL returning JSON with a `MAX_PARALLEL_CHUNKS` field |
| `ACCOUNT_MAX_CHUNKS_DEFAULT` | `5` | Last-resort default chunk limit |
| `RATE_LIMIT` | `10` | Requests per second per client IP; `0` disables. Excess requests get `429` with `Retry-After` |
| `TRUSTED_PROXIES` | _(empty)_ | Comma-separated CIDRs whose `X-Forwarded-For` is trusted for the client IP; empty = TCP peer address |

### CLI Commands

Print account data as JSON:

    account <account_id>

Hard-delete every reservation for the account:

    clear-reservations <account_id>
