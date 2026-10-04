# Ticket Reservation API — Railway Deployment Test Guide

This guide tests the deployed API end to end: health/readiness, admin-only show creation, show reconciliation, reservations, idempotency, cancellation, ownership checks, and metrics.

## Important credential handling

The test JWT signing secret below can mint admin tokens for this deployment. Share this guide privately with the recruiter; do not commit it to a public repository or publish it. Rotate `JWT_SECRET` in Railway after the evaluation and update this guide or remove the secret. The MariaDB credentials are not needed for API testing and are intentionally not included.

## Prerequisites

- The app and MariaDB deployments are running in Railway.
- Railway `/readyz` returns `200` (migrations have completed and the database is reachable).
- `curl` and Python 3 are installed on the test machine.
- Run all shell commands below in one Bash session so the variables remain set.

## 1. Configure the deployed app URL and JWT secret

Use the app's public Railway domain, not the MariaDB private domain. Remove any trailing slash so requests do not become `//livez` and receive a redirect.

```bash
BASE_URL='https://ticket-reservation-production-e646.up.railway.app'
BASE_URL="${BASE_URL%/}"

export JWT_SECRET='refer mail'
printf 'JWT secret set: %s; length: %s\n' "${JWT_SECRET:+yes}" "${#JWT_SECRET}"
```

Expected secret length is 64. If the Railway secret has been rotated, replace the value above with the current value configured on the app service.

## 2. Create test JWTs

The API uses HS256 JWTs. The token `sub` becomes the caller's user ID. The show creation route requires `role=admin`; reserve and cancel require a valid authenticated token. `ADMIN_USERNAME` and `ADMIN_PASSWORD` are required app configuration values, but there is no login endpoint that exchanges them for JWTs.

```bash
make_jwt() {
  JWT_SUB="$1" JWT_ROLE="$2" python3 - <<'PY'
import base64, hashlib, hmac, json, os, time

b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
header = b64(json.dumps({"alg":"HS256","typ":"JWT"}, separators=(",", ":")).encode())
claims = b64(json.dumps({
    "sub": os.environ["JWT_SUB"],
    "role": os.environ["JWT_ROLE"],
    "exp": int(time.time()) + 3600
}, separators=(",", ":")).encode())
message = f"{header}.{claims}"
signature = b64(hmac.new(os.environ["JWT_SECRET"].encode(), message.encode(), hashlib.sha256).digest())
print(f"{message}.{signature}")
PY
}

ADMIN_TOKEN="$(make_jwt admin-user admin)"
USER_TOKEN="$(make_jwt user-1 user)"
USER_B_TOKEN="$(make_jwt user-2 user)"
printf 'Admin token length: %s; user token length: %s\n' "${#ADMIN_TOKEN}" "${#USER_TOKEN}"
```

Tokens are valid for one hour. If a request later returns `401`, generate fresh tokens using the current `JWT_SECRET`.

## 3. Test liveness, readiness, and metrics

```bash
curl -i "$BASE_URL/livez"
curl -i "$BASE_URL/readyz"
curl -sS "$BASE_URL/metrics" | grep -E '^(reservations_|seats\{status=)'
```

Expected:

- `/livez` returns `200` and checks the process only.
- `/readyz` returns `200` and checks database readiness.
- `/metrics` returns Prometheus text, including reservation counters and DB-derived seat gauges.

## 4. Test admin authorization and create a show

No token must return `401`; a valid non-admin token must return `403`:

```bash
curl -i -X POST "$BASE_URL/shows" \
  -H 'Content-Type: application/json' -d '{}'

curl -i -X POST "$BASE_URL/shows" \
  -H "Authorization: Bearer $USER_TOKEN" \
  -H 'Content-Type: application/json' -d '{}'
```

Create a five-seat show with the admin token and retain the returned show ID:

```bash
SHOW_JSON="$(curl -sS -X POST "$BASE_URL/shows" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Recruiter end-to-end test","seats":["A1","A2","A3","A4","A5"],"price_paise":25000}')"

printf '%s\n' "$SHOW_JSON"
SHOW_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' <<<"$SHOW_JSON")"
printf 'Show ID: %s\n' "$SHOW_ID"
```

Expected: `201` and a nonempty show ID. Each new test run creates a new show; prior shows remain in the Railway database.

## 5. Test show details and reconciliation

```bash
curl -sS "$BASE_URL/shows/$SHOW_ID" | python3 -m json.tool
curl -sS "$BASE_URL/shows/$SHOW_ID?summary=true" | python3 -m json.tool
curl -i "$BASE_URL/shows/00000000-0000-4000-8000-000000000000"
```

Expected:

- Full show response has flat counts and five ordered `seats` entries, each containing `seat_id` and `status`.
- `?summary=true` keeps the flat fields and counts but omits `seats`.
- Unknown show ID returns JSON `404`.

## 6. Test reserve success, identity, and idempotency

Reserve `A1` and `A2` with a unique idempotency key. The spoofed body `user_id` should be ignored; the response should identify `user-1` from the JWT.

```bash
IDEM_KEY="recruiter-e2e-$(date +%s)"

RESERVE_JSON="$(curl -sS -X POST "$BASE_URL/shows/$SHOW_ID/reserve" \
  -H "Authorization: Bearer $USER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $IDEM_KEY" \
  -d '{"seats":["A1","A2"],"user_id":"spoofed-user"}')"

printf '%s\n' "$RESERVE_JSON"
RESERVATION_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["reservation_id"])' <<<"$RESERVE_JSON")"
printf 'Reservation ID: %s\n' "$RESERVATION_ID"
```

Expected: `201`, `status` is `confirmed`, seats are normalized, and `user_id` is `user-1`.

Replay the same request using the same token, key, and seats. Expect `201`, the same response body, and `Idempotent-Replayed: true`:

```bash
curl -i -X POST "$BASE_URL/shows/$SHOW_ID/reserve" \
  -H "Authorization: Bearer $USER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $IDEM_KEY" \
  -d '{"seats":["A1","A2"],"user_id":"spoofed-user"}'
```

Using the same key with a different seat list must return `409 idempotency_key_conflict`:

```bash
curl -i -X POST "$BASE_URL/shows/$SHOW_ID/reserve" \
  -H "Authorization: Bearer $USER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $IDEM_KEY" \
  -d '{"seats":["A3"]}'
```

Another user trying to reserve already-confirmed `A1` must receive `409`:

```bash
curl -i -X POST "$BASE_URL/shows/$SHOW_ID/reserve" \
  -H "Authorization: Bearer $USER_B_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: user-2-taken-seat' \
  -d '{"seats":["A1"]}'
```

The default per-user limit is four. User 1 already reserved two seats, so requesting three additional seats must return `409`:

```bash
curl -i -X POST "$BASE_URL/shows/$SHOW_ID/reserve" \
  -H "Authorization: Bearer $USER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: user-1-limit-check' \
  -d '{"seats":["A3","A4","A5"]}'
```

## 7. Test cancellation, owner checks, and replay safety

Cancel user 1's reservation. Expect `200` with `status: cancelled`. Repeating the cancellation must again return `200` with the same response:

```bash
curl -i -X POST "$BASE_URL/reservations/$RESERVATION_ID/cancel" \
  -H "Authorization: Bearer $USER_TOKEN"

curl -i -X POST "$BASE_URL/reservations/$RESERVATION_ID/cancel" \
  -H "Authorization: Bearer $USER_TOKEN"
```

Replay the original reserve request with its old key. It should replay its frozen original `201` response, without booking the seats again:

```bash
curl -i -X POST "$BASE_URL/shows/$SHOW_ID/reserve" \
  -H "Authorization: Bearer $USER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $IDEM_KEY" \
  -d '{"seats":["A1","A2"],"user_id":"spoofed-user"}'
```

Now user 2 should be able to reserve released seat `A1`:

```bash
B_RESERVE_JSON="$(curl -sS -X POST "$BASE_URL/shows/$SHOW_ID/reserve" \
  -H "Authorization: Bearer $USER_B_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: user-2-after-cancel' \
  -d '{"seats":["A1"]}')"

printf '%s\n' "$B_RESERVE_JSON"
B_RESERVATION_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["reservation_id"])' <<<"$B_RESERVE_JSON")"
```

Replay the old cancellation as user 1. It must not free user 2's new `A1` booking. Then try cancelling user 2's reservation as user 1; expect `404` because user 1 is not the owner:

```bash
curl -i -X POST "$BASE_URL/reservations/$RESERVATION_ID/cancel" \
  -H "Authorization: Bearer $USER_TOKEN"

curl -i -X POST "$BASE_URL/reservations/$B_RESERVATION_ID/cancel" \
  -H "Authorization: Bearer $USER_TOKEN"

curl -sS "$BASE_URL/shows/$SHOW_ID" | python3 -m json.tool
```

In the final show body, `A1` must still be `confirmed`. Cancel user 2's reservation as its owner to return it to available:

```bash
curl -i -X POST "$BASE_URL/reservations/$B_RESERVATION_ID/cancel" \
  -H "Authorization: Bearer $USER_B_TOKEN"
```

## 8. Check deployed metrics and logs

```bash
curl -sS "$BASE_URL/metrics" | grep -E '^(http_requests_total|reservations_|seats\{status=)'
```

The seat gauge in `/metrics` covers all shows in the database. Application JSON logs are in Railway under the app service's **Deployments → latest deployment → Logs**.

## Expected status summary

| Test | Expected |
| --- | --- |
| `GET /livez` | `200` |
| `GET /readyz` | `200` |
| `POST /shows` without token | `401` |
| `POST /shows` with user token | `403` |
| `POST /shows` with admin token | `201` |
| `GET /shows/{id}` | `200`, ordered seats and matching counts |
| `GET /shows/{id}?summary=true` | `200`, no `seats` property |
| First reserve | `201` |
| Same reserve key and body replay | `201`, `Idempotent-Replayed: true` |
| Same reserve key, different seat list | `409 idempotency_key_conflict` |
| Another user reserves a taken seat | `409` |
| Per-user limit exceeded | `409` |
| Owner cancel | `200`, status `cancelled` |
| Unknown or non-owner cancel | `404` |
| Old cancel replay after another user reserves the seat | `200`; new user's seat stays confirmed |

