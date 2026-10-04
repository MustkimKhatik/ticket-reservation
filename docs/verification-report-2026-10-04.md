# Final verification report — 2026-10-04

All checks ran against local Docker Compose / MariaDB 11.4. The deployment was not load-tested; burst results below are from the local stack.

## 1. Reservation burst

`make burst BASE_URL=http://localhost:8080 JWT_SECRET=<local-test-secret> OUT=/tmp/burst-run-N.md` was run twice. Both runs completed 20,762 requests and passed the tool's reconciliation and metric-delta assertions.

| Result | Run 1 | Run 2 |
| --- | ---: | ---: |
| 201 responses (includes idempotent replays) | 19,256 | 19,256 |
| 409 `seat_taken` | 1,399 | 1,399 |
| 409 `per_user_limit_exceeded` | 6 | 6 |
| 409 `idempotency_key_conflict` | 1 | 1 |
| Other 4xx | 0 | 0 |
| Other 2xx (cancels) | 100 | 100 |
| 5xx / network errors | 0 / 0 | 0 / 0 |
| Idempotent replay responses (subset of 201) | 19,050 | 19,050 |
| Latency p50 / p95 / p99 | 65.76 / 126.37 / 164.88 ms | 65.74 / 129.08 / 168.42 ms |
| Throughput | 1,555.81 req/s | 1,539.19 req/s |

Each hot-seat storm yielded exactly one 201 and 499 `seat_taken` conflicts. Each 20,000-request stampede used 1,000 unique skewed requests (100 seats, 100 confirmed) and 19,000 same-key retries (all replays). The limit scenario returned four 201 and six limit conflicts; the same-key/different-body check returned one `idempotency_key_conflict`; 50 cancel/rebook cycles completed.

Counter deltas matched client outcomes in both runs: confirmed 206, `seat_taken` 1,399, `per_user_limit` 6, and `idempotent_replay` 19,050. Final reconciliation for every test show matched all expected client-side states and the invariant:

| Scenario | Available | Held | Confirmed | Total / listed |
| --- | ---: | ---: | ---: | ---: |
| Hot seat | 0 | 0 | 1 | 1 / 1 |
| 20k stampede | 0 | 0 | 100 | 100 / 100 |
| Per-user limit | 6 | 0 | 4 | 10 / 10 |
| Idempotency | 2 | 0 | 1 | 3 / 3 |
| Cancel/rebook churn | 2 | 0 | 0 | 2 / 2 |

## 2. Readiness fail-closed check

`make readiness-check` printed:

```text
PASS: initial /readyz returns 200 — last status=200
PASS: initial /livez returns 200 — last status=200
PASS: /readyz fails closed within 10s while DB is stopped — last status=503
PASS: /livez stays 200 while DB is stopped — status=200
PASS: /readyz returns 200 within 60s after DB starts — last status=200
PASS: app container was not restarted — before=e2c14ced82fa after=e2c14ced82fa
```

## 3. Clean-clone check

`make verify-clean-clone` cloned committed `HEAD` to a temporary directory, built and started its isolated Compose stack, reached `/readyz=200`, ran `go test ./...` successfully (`ok ticket-reservation 1.473s`), passed the short 1,000-request stampede burst plus contention/idempotency/limit/churn checks, and removed the temporary stack and directory. The short-burst reconciliation and metric deltas matched. The existing local Compose service was not used by this check.

## 4. Log and anomaly review

The two full bursts had no 5xx or network errors. The recent local app/MariaDB logs had no deadlock, lock-wait-timeout, panic, or unexpected 5xx lines. The readiness test's 503 `/readyz` responses while MariaDB was stopped were expected. Burst p95 and p99 stayed close between runs; no sharp latency spike was observed.

Two initial verification attempts exposed test-tool issues, not service failures: the burst metrics parser expected JSON instead of Prometheus text, and the readiness script did not replace empty exported credentials. Both test tools were corrected before the successful runs above; no service logic was changed.
