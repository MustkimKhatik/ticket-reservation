#!/usr/bin/env python3
"""End-to-end smoke test for the deployed ticket reservation API."""

import hashlib
import hmac
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.request
from base64 import urlsafe_b64encode


def fail(message):
    print(f"FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


BASE_URL = os.environ.get("BASE_URL", "").rstrip("/")
JWT_SECRET = os.environ.get("JWT_SECRET", "")
if not BASE_URL:
    fail("BASE_URL is required. Example: make deployment-smoke BASE_URL=https://your-app.up.railway.app JWT_SECRET=...")
if not JWT_SECRET:
    fail("JWT_SECRET is required. Pass the current app JWT secret to make.")

NONCE = secrets.token_hex(6)
USER_ID = f"smoke-user-{NONCE}"
USER_B_ID = f"smoke-user-b-{NONCE}"


def b64url(value):
    return urlsafe_b64encode(value).rstrip(b"=").decode("ascii")


def make_jwt(subject, role):
    header = b64url(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
    claims = b64url(json.dumps({
        "sub": subject,
        "role": role,
        "exp": int(time.time()) + 3600,
    }, separators=(",", ":")).encode())
    message = f"{header}.{claims}"
    signature = hmac.new(JWT_SECRET.encode(), message.encode(), hashlib.sha256).digest()
    return f"{message}.{b64url(signature)}"


ADMIN_TOKEN = make_jwt(f"smoke-admin-{NONCE}", "admin")
USER_TOKEN = make_jwt(USER_ID, "user")
USER_B_TOKEN = make_jwt(USER_B_ID, "user")


def request(method, path, token=None, body=None, headers=None):
    request_headers = dict(headers or {})
    if token:
        request_headers["Authorization"] = f"Bearer {token}"
    data = None
    if body is not None:
        data = json.dumps(body, separators=(",", ":")).encode()
        request_headers["Content-Type"] = "application/json"
    req = urllib.request.Request(BASE_URL + path, data=data, headers=request_headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            return response.status, response.read(), response.headers
    except urllib.error.HTTPError as error:
        return error.code, error.read(), error.headers
    except Exception as error:  # Network, TLS, and timeout errors should be actionable.
        fail(f"{method} {path}: request failed: {error}")


def expect(method, path, expected, **kwargs):
    status, body, headers = request(method, path, **kwargs)
    if status != expected:
        fail(f"{method} {path}: expected HTTP {expected}, got {status}: {body.decode(errors='replace')}")
    return body, headers


def json_body(body, operation):
    try:
        return json.loads(body)
    except Exception as error:
        fail(f"{operation}: response was not JSON: {error}; body={body.decode(errors='replace')}")


def check_show(show_id, expected_available, expected_held, expected_confirmed):
    body, _ = expect("GET", f"/shows/{show_id}", 200)
    show = json_body(body, "show reconciliation")
    seats = show.get("seats")
    expected_counts = {
        "available": expected_available,
        "held": expected_held,
        "confirmed": expected_confirmed,
    }
    if not isinstance(seats, list) or len(seats) != show.get("total_seats"):
        fail(f"show reconciliation: seats length does not equal total_seats: {show}")
    if any(show.get(key) != value for key, value in expected_counts.items()):
        fail(f"show reconciliation: unexpected counts: {show}")
    derived = {status: sum(seat.get("status") == status for seat in seats)
               for status in ("available", "held", "confirmed")}
    if derived != expected_counts or sum(expected_counts.values()) != show["total_seats"]:
        fail(f"show reconciliation: listed seats and flat counts disagree: {show}")
    if [seat["seat_id"] for seat in seats] != sorted(seat["seat_id"] for seat in seats):
        fail("show reconciliation: seat list is not ordered by seat_id")
    if any(set(seat) != {"seat_id", "status"} for seat in seats):
        fail("show reconciliation: per-seat objects should contain only seat_id and status")
    print(f"PASS show reconciliation: available={expected_available}, held={expected_held}, confirmed={expected_confirmed}")
    return show


print("1/8 Health and readiness")
expect("GET", "/livez", 200)
expect("GET", "/readyz", 200)
print("PASS /livez and /readyz returned 200")

print("2/8 Authentication")
expect("POST", "/shows", 401, body={})
expect("POST", "/shows", 403, token=USER_TOKEN, body={})
print("PASS missing token=401; non-admin token=403")

print("3/8 Create show and reconcile")
show_body = {
    "name": f"Deployment smoke {NONCE}",
    "seats": ["A1", "A2", "A3", "A4", "A5"],
    "price_paise": 25000,
}
body, _ = expect("POST", "/shows", 201, token=ADMIN_TOKEN, body=show_body)
created_show = json_body(body, "create show")
show_id = created_show.get("id")
if not show_id:
    fail(f"create show returned no id: {created_show}")
check_show(show_id, 5, 0, 0)
summary_body, _ = expect("GET", f"/shows/{show_id}?summary=true", 200)
summary = json_body(summary_body, "show summary")
if "seats" in summary:
    fail("GET ?summary=true should omit seats")
expect("GET", "/shows/00000000-0000-4000-8000-000000000000", 404)
expect("POST", "/shows", 400, token=ADMIN_TOKEN, body={
    "name": "duplicate validation",
    "seats": ["a1", " A1 "],
    "price_paise": 25000,
})
print(f"PASS create show, full/summary GET, unknown show 404, duplicate validation 400 (show_id={show_id})")

print("4/8 Reserve, JWT identity, replay, conflicts, and limit")
idem_key = f"deployment-smoke-{NONCE}"
reserve_body = {"seats": ["A1", "A2"], "user_id": "spoofed-user"}
reserve_headers = {"Idempotency-Key": idem_key}
reserve_response, _ = expect("POST", f"/shows/{show_id}/reserve", 201,
                             token=USER_TOKEN, body=reserve_body, headers=reserve_headers)
reservation = json_body(reserve_response, "reserve")
reservation_id = reservation.get("reservation_id")
if not reservation_id or reservation.get("user_id") != USER_ID or reservation.get("status") != "confirmed":
    fail(f"reserve did not use JWT identity or return confirmed booking: {reservation}")
if reservation.get("seats") != ["A1", "A2"]:
    fail(f"reserve returned unexpected normalized seats: {reservation}")
check_show(show_id, 3, 0, 2)

replay_body, replay_headers = expect("POST", f"/shows/{show_id}/reserve", 201,
                                     token=USER_TOKEN, body=reserve_body, headers=reserve_headers)
if replay_body != reserve_response or replay_headers.get("Idempotent-Replayed", "").lower() != "true":
    fail("same-key reserve replay did not return the frozen response and replay header")
conflict_body, _ = expect("POST", f"/shows/{show_id}/reserve", 409, token=USER_TOKEN,
                          body={"seats": ["A3"]}, headers=reserve_headers)
if json_body(conflict_body, "idempotency conflict").get("error", {}).get("code") != "idempotency_key_conflict":
    fail("same key with different seats did not return idempotency_key_conflict")
taken_body, _ = expect("POST", f"/shows/{show_id}/reserve", 409, token=USER_B_TOKEN,
                       body={"seats": ["A1"]}, headers={"Idempotency-Key": f"taken-{NONCE}"})
if json_body(taken_body, "taken seat").get("error", {}).get("code") != "seat_taken":
    fail("another user's taken-seat request did not return seat_taken")
limit_body, _ = expect("POST", f"/shows/{show_id}/reserve", 409, token=USER_TOKEN,
                       body={"seats": ["A3", "A4", "A5"]}, headers={"Idempotency-Key": f"limit-{NONCE}"})
if json_body(limit_body, "per-user limit").get("error", {}).get("code") != "per_user_limit_exceeded":
    fail("request exceeding per-user limit did not return per_user_limit_exceeded")
print("PASS first reserve, JWT-only identity, idempotent replay, key conflict, taken seat, and per-user limit")

print("5/8 Owner cancellation and reserve replay")
cancel_path = f"/reservations/{reservation_id}/cancel"
cancel_body, _ = expect("POST", cancel_path, 200, token=USER_TOKEN, body={"user_id": "spoofed-user"})
cancelled = json_body(cancel_body, "cancel")
if cancelled.get("status") != "cancelled" or cancelled.get("seats") != ["A1", "A2"]:
    fail(f"cancel response was unexpected: {cancelled}")
cancel_replay, _ = expect("POST", cancel_path, 200, token=USER_TOKEN)
if cancel_replay != cancel_body:
    fail("cancel replay response differs from first cancellation")
reserve_replay, reserve_replay_headers = expect("POST", f"/shows/{show_id}/reserve", 201,
                                                 token=USER_TOKEN, body=reserve_body, headers=reserve_headers)
if reserve_replay != reserve_response or reserve_replay_headers.get("Idempotent-Replayed", "").lower() != "true":
    fail("original reserve key did not replay its frozen response after cancel")
check_show(show_id, 5, 0, 0)
print("PASS owner cancel, same-body cancel replay, frozen reserve replay, and seats remain available")

print("6/8 Old-cancel replay safety and ownership")
body, _ = expect("POST", f"/shows/{show_id}/reserve", 201, token=USER_B_TOKEN,
                 body={"seats": ["A1"]}, headers={"Idempotency-Key": f"user-b-{NONCE}"})
booking_b = json_body(body, "user B reserve")
reservation_b_id = booking_b.get("reservation_id")
if not reservation_b_id:
    fail(f"user B reserve returned no reservation id: {booking_b}")
old_cancel_replay, _ = expect("POST", cancel_path, 200, token=USER_TOKEN)
if old_cancel_replay != cancel_body:
    fail("old cancel replay body differs from the first cancel")
show = check_show(show_id, 4, 0, 1)
if next(seat for seat in show["seats"] if seat["seat_id"] == "A1")["status"] != "confirmed":
    fail("replaying old cancel freed a seat now owned by user B")
non_owner_body, _ = expect("POST", f"/reservations/{reservation_b_id}/cancel", 404, token=USER_TOKEN)
unknown_body, _ = expect("POST", "/reservations/00000000-0000-4000-8000-000000000000/cancel", 404, token=USER_TOKEN)
if json_body(non_owner_body, "non-owner cancel") != json_body(unknown_body, "unknown cancel"):
    fail("non-owner and unknown reservation responses should match")
expect("POST", f"/reservations/{reservation_b_id}/cancel", 200, token=USER_B_TOKEN)
check_show(show_id, 5, 0, 0)
print("PASS old cancel cannot free a later booking; owner check hides reservation existence")

print("7/8 Prometheus metrics")
metrics_body, _ = expect("GET", "/metrics", 200)
metrics = metrics_body.decode(errors="replace")
for metric in ("reservations_confirmed_total", "reservations_cancelled_total", "reservations_declined_total", 'seats{status="available"}'):
    if metric not in metrics:
        fail(f"/metrics is missing {metric}")
print("PASS metrics include reservation counters and DB-derived seats gauge")

print("8/8 All deployed API smoke checks passed")
