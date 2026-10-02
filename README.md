# Ticket reservation foundation

Block 1 is a Go HTTP service using MariaDB with InnoDB. It provides retrying startup migrations, process liveness and database readiness checks, request IDs and JSON logs, Prometheus metrics, HS256 JWT authentication, admin-only `POST /shows`, and a one-query show/seat reconciliation endpoint. Reservation, cancellation, holds, expiry, and idempotency are intentionally not implemented yet.

## Deploy with Docker Compose

From the cloned repository, set production secrets, build the image, and start both services:

```sh
export JWT_SECRET="$(openssl rand -hex 32)"
export ADMIN_USERNAME='admin'
export ADMIN_PASSWORD="$(openssl rand -hex 24)"
export DB_PASSWORD="$(openssl rand -hex 24)"
export MARIADB_ROOT_PASSWORD="$(openssl rand -hex 24)"
docker compose up -d --build
```

Compose publishes the app on port 8080 and binds MariaDB's development port only to `127.0.0.1:3306`. Set `PORT` to change the host app port. The app starts serving liveness immediately, retries database ping/migration with exponential backoff, and becomes ready only after migrations succeed. Configure the platform's liveness probe for `GET /livez` and readiness probe for `GET /readyz`.

View structured JSON application logs and metrics with:

```sh
docker compose logs -f app
curl http://localhost:8080/metrics
```

`/metrics` exposes request counters and latency histograms by route/status class, `reservations_confirmed_total`, `reservations_declined_total` with `reason` values `seat_taken`, `per_user_limit`, or `idempotent_replay`, DB pool statistics, and live `seats{status=...}` counts. Seat counts are queried from MariaDB with `SELECT status, COUNT(*) FROM seats GROUP BY status` at each scrape. `/readyz` pings the DB with a short timeout and returns 503 until the startup migration completes or whenever the DB is unavailable. `/livez` only checks that the process can serve HTTP and never touches the DB.

Create a show with an admin bearer token:

```sh
curl -X POST http://localhost:8080/shows \
  -H 'Authorization: Bearer <HS256-JWT>' \
  -H 'Content-Type: application/json' \
  -d '{"name":"Friday show","seats":["A1","A2"],"price_paise":25000}'
```

JWT claims used by this block: `sub` is the user ID, `role` must equal `admin` for this route, and `exp` is an optional Unix expiry. The signing algorithm must be HS256. The service verifies identity from the signed token only; it does not use any identity from the body.

Fetch show fields and a seat-state reconciliation snapshot:

```sh
curl http://localhost:8080/shows/<show-id>
```

The response includes `available`, `held`, `confirmed`, and `total_seats`. The fields and counts come from one MariaDB statement so the counts share a single InnoDB statement snapshot.

## Environment

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `DB_DSN` | yes | — | Go MySQL driver DSN for MariaDB; use `parseTime=true&charset=utf8mb4` |
| `JWT_SECRET` | yes | — | Shared HS256 signing secret |
| `ADMIN_USERNAME` | yes | — | Reserved deployment/admin credential setting |
| `ADMIN_PASSWORD` | yes | — | Reserved deployment/admin credential setting |
| `PORT` | no | `8080` | HTTP listen port |
| `DB_POOL_MAX_OPEN` | no | `100` | Maximum open SQL connections |
| `DB_POOL_MAX_IDLE` | no | `50` | Maximum idle SQL connections |
| `DB_CONNECTION_LIMIT` | no | `400` | MariaDB connection limit configured for this deployment; app pool max open must be lower |
| `DB_CONN_MAX_LIFETIME` | no | `5m` | Maximum lifetime of a pooled connection |
| `DB_STATEMENT_TIMEOUT` | no | `5s` | Per-query timeout after acquiring a connection |
| `DB_ACQUIRE_TIMEOUT` | no | `30s` | Maximum wait for a pooled connection; requests wait instead of immediately failing |
| `REQUEST_TIMEOUT` | no | `60s` | HTTP read/write and request deadline; must exceed DB acquire timeout |
| `READY_TIMEOUT` | no | `2s` | Short readiness ping deadline |
| `DB_STARTUP_TIMEOUT` | no | `5s` | Timeout for each startup ping/migration attempt before retry/backoff |

Choose the pool size against the MariaDB `max_connections` budget across all app replicas. Compose sets MariaDB max connections to 400 and app pool maximum to 100, leaving headroom. Across multiple replicas, set each pool maximum so their combined capacity remains under the MariaDB connection limit. Requests wait up to `DB_ACQUIRE_TIMEOUT`; pool deadlines, MariaDB connection-limit errors, lock timeouts, and deadlocks return a clean 429 decline. Other DB errors return a clean 503. Driver details do not leak into responses. The DSN's driver `timeout`, `readTimeout`, and `writeTimeout` further bound connection and socket operations.

## Schema and atomicity decisions

Startup creates `shows` and `seats` using InnoDB and utf8mb4. Show IDs are app-generated UUIDs. A show is inserted with its immutable `total_seats`, followed by all seat rows in one transaction. Seat rows have a composite `(show_id, seat_id)` primary key, binary seat collation, and a `(show_id, status)` index. This supports exact seat identity and future row-level conditional updates/ordered locks. No availability counters exist; counts derive from seat rows. A failed seat batch rolls back both the show and its seats. Seat creation is synchronous, batched at 1,000 values per insert, and the response is written only after commit.

The schema includes nullable `user_id`, `reservation_id`, and `held_until` seat fields to support later reservation and hold blocks. It deliberately does not create reservation or idempotency tables in Block 1. Application validation caps shows at 50,000 seats and normalizes seat IDs with trim plus uppercase before duplicate detection and storage. Normalized seat IDs are limited to 16 Unicode characters, matching the `VARCHAR(16)` column.

## Local development and tests

Set `DB_DSN`, `JWT_SECRET`, `ADMIN_USERNAME`, and `ADMIN_PASSWORD`, then run:

```sh
go run .
go test ./...
```

HTTP/auth/validation tests run without a database. MariaDB integration tests use `TEST_DB_DSN`, or the `DB_USER`, `DB_PASSWORD`, `DB_HOST`, and `DB_NAME` variables (defaults target the Compose database). They skip when MariaDB is unavailable. The 10,000-seat test verifies HTTP 201, reconciliation counts, readiness, and database-derived metrics; other integration tests check pool wait/decline behavior and transaction rollback.
