# WRITEUP: Seat Reservation at Scale

Live URL: `https://ticket-reservation-production-e646.up.railway.app/`
Burst: `make burst BASE_URL=http://localhost:8080 JWT_SECRET="$JWT_SECRET" OUT=docs/burst-local.md` (see README)

## 1. The atomic decision

**Mechanism:** a conditional `UPDATE` on the seat row, with the outcome decided by rows-affected.

```sql
UPDATE seats
SET status='confirmed', user_id=?, reservation_id=?
WHERE show_id=? AND seat_id IN (...) AND status='available';
```

If `rows_affected != number of seats requested`, the transaction rolls back and the request gets a 409 `seat_taken`. Application code never reads a seat's state and then decides.

**Why it is race-free:** each seat is one row with a composite primary key `(show_id, seat_id)`, and the check and the write happen in the same statement. InnoDB takes an exclusive lock on the matched row. When 500 requests race for A12, they queue on that lock. The winner commits and sets `status='confirmed'`. Each loser then re-evaluates its `WHERE` against the committed row, finds `status != 'available'`, matches zero rows, and declines. There is no window between "check" and "write" for a second winner to slip into.

**Multi-seat requests are all-or-nothing.** If any requested seat is not free, nothing is reserved and the response is 409. I chose this over best-effort because a partial result complicates idempotent replay (what would a retry return?) and forces clients to handle partial success. It holds under concurrency because the whole request is one statement inside one transaction: either every seat flips or none does.

**Deadlock avoidance:** the seat update is a single statement, so InnoDB locks the matching rows in primary-key order, and every multi-seat request locks in that same order. Across the whole transaction the order is always: idempotency row, per-user lock row, seat rows. Two requests for `["A1","A2"]` and `["A2","A1"]` therefore cannot form a cycle. As a backstop, MariaDB error 1213 (deadlock) is retried up to 3 times with jittered backoff, and 1205 (lock wait timeout) is mapped to a clean response, never a raw 500.

**Per-user limit:** a small `user_show_locks` row per `(show, user)` is locked with `SELECT ... FOR UPDATE`, then the user's confirmed seats are counted from the seat rows. The lock serializes one user's parallel requests, so ten parallel reserves on a limit-4 show cannot all pass the count. Counts come from seat rows, not a separate counter, so they cannot drift. Result in testing: 4 confirmed, 6 declined with `per_user_limit_exceeded`.

**Cancel** uses the same idea in reverse. The release is guarded by who owns the seat now:

```sql
UPDATE seats SET status='available', user_id=NULL, reservation_id=NULL
WHERE show_id=? AND reservation_id=? AND status='confirmed';
```

Matching on `reservation_id` (never on seat id alone) is what stops a stale or repeated cancel from resurrecting a seat that has since been re-booked: the new booking carries a different reservation id, so the old cancel matches zero rows. This is covered by a test (cancel, another user books the seat, replay the old cancel, the seat stays confirmed).

## 2. Idempotency

- **Where the key lives:** table `idempotency_keys`, unique on `(user_id, idem_key)`. It stores a request hash (SHA-256 of show id plus the sorted, normalized seats), the stored response body, and the status code.
- **Exactly-once:** the key row is inserted in the same transaction as the reservation, as the first step. A unique-key conflict means another request already claimed it. Because the key and the reservation commit or roll back together, a crash cannot leave a reservation without its key, or a key without its reservation.
- **Retry:** same key and same hash returns the stored response (201, same body, with an `Idempotent-Replayed: true` header). Replays are counted separately in metrics.
- **Same key, different body:** hash differs, so 409 `idempotency_key_conflict`.
- **Declines are not stored.** A 409 rolls the transaction back, so the key is not consumed; a retry re-evaluates and can succeed if the seat has since been freed.
- **Replays are frozen.** After a cancel, replaying the original reserve call returns the original stored response; it never creates a second booking.
- **Scope:** keys are per user, so two users cannot collide on the same key.

## 3. Holds and expiry

I chose the **explicit cancel** model (`POST /reservations/{id}/cancel`, owner only) and no auto-expiring holds. Reasons: no background job, no clock-dependent behavior during a burst, and fewer states to reconcile. Reserve moves a seat straight from `available` to `confirmed`; the `held` state exists in the schema and invariant (always 0 today).

Cancel details: non-owner and unknown ids both return 404 so existence is not leaked; a repeat cancel returns 200 with the same body and moves nothing; cancel releases all seats of the reservation, never partially.

If I added expiring holds, I would set `status='held'` plus `held_until` on reserve, and expire them with the same kind of guarded update: `UPDATE ... SET status='available' WHERE status='held' AND held_until < NOW()`, so an expiry can never touch a seat that was confirmed in the meantime.

## 4. Consistency vs availability under a partition

There is a single database and a single source of truth, so the service chooses **consistency**. If the database is unreachable, `/readyz` returns 503 (it fails closed) and the platform stops routing traffic to the instance; `/livez` does not touch the database, so a database outage does not cause pointless restarts. I would rather refuse a request than risk selling a seat on stale or uncertain state. The cost is availability during a database outage. Multi-region or replicated writes would need a real consensus story (or a single writer), which I did not attempt in a one-day scope.

Under load the same preference applies: requests queue for a pooled connection instead of failing, so overload shows up as latency, not as errors or wrong answers.

## 5. Observability

- **Health:** `/livez` (process only) and `/readyz` (DB ping, 503 when down).
- **Metrics (`/metrics`, Prometheus):** `reservations_confirmed_total`, `reservations_declined_total{reason=seat_taken|per_user_limit|idempotent_replay}`, `reservations_cancelled_total`, and a seats gauge by status computed from the database at scrape time (briefly cached), so it reconciles with `GET /shows/{id}` and survives restarts. Counters increment only after commit.
- **Logs:** structured JSON with a request id on every line and in the `X-Request-ID` response header. 
- **What I would be paged for at 2am:**
  - any sustained 5xx rate above zero
  - `/readyz` failing
  - the invariant check mismatching (available + held + confirmed != total_seats)
  - DB pool wait time or in-use connections near the limit
  - p99 latency rising toward the proxy timeout (that is where 502/504 begin)
  - a rising deadlock or lock-wait retry rate
  - an unusual `per_user_limit` or `idempotent_replay` rate (a client bug or abuse)

## 6. Results

Local burst (docker-compose), one run: 20,762 requests at about 1,640 req/s, p50 62 ms, p95 116 ms, p99 149 ms.
- Hot-seat storm: 500 users, one seat: **1 x 201, 499 x 409**.
- 0 x 5xx, 0 network errors.
- Invariant held on every show (available + held + confirmed == total_seats == listed seats).
- Metric deltas matched client-side counts exactly (206 confirmed, 1,399 seat-taken, 6 per-user-limit, 19,050 idempotent replays).


**Known limitations, stated honestly:**
- The "stampede" scenario in my burst tool is mostly same-key replays (about 19,000 of 20,000 requests); only about 1,000 were unique reservation attempts. It exercises idempotency heavily but seat contention less than a stampede of unique keys would.
- Metrics counters are per-process and reset on restart, so I run a single instance.

## 7. AI usage (directed vs decided)
Claude (chat) as a design partner and prompt writer; Codex as the implementation agent, for each requirement I asked Claude for how to achieve it, what could break it, the available approaches, and a recommendation, and then turned that into a scoped prompt for Codex, one block at a time, with the six correctness bars stated up front as non-negotiable. I asked Codex for a plan and objections before it wrote code.
