#!/usr/bin/env python3
"""Concurrent end-to-end reservation load and reconciliation check."""

import argparse
import concurrent.futures
import hashlib
import hmac
import json
import os
import secrets
import statistics
import sys
import time
import urllib.error
import urllib.request
from collections import Counter
from datetime import datetime, timezone


def jwt(secret, sub, role):
    import base64

    enc = lambda value: base64.urlsafe_b64encode(value).rstrip(b"=").decode()
    head = enc(b'{"alg":"HS256","typ":"JWT"}')
    claims = enc(json.dumps({"sub": sub, "role": role, "exp": int(time.time()) + 3600}, separators=(",", ":")).encode())
    message = f"{head}.{claims}"
    signature = enc(hmac.new(secret.encode(), message.encode(), hashlib.sha256).digest())
    return f"{message}.{signature}"


def call(method, path, token=None, payload=None, key=None, timeout=90):
    url = BASE_URL + path
    headers = {}
    body = None
    if token:
        headers["Authorization"] = "Bearer " + token
    if payload is not None:
        headers["Content-Type"] = "application/json"
        body = json.dumps(payload, separators=(",", ":")).encode()
    if key:
        headers["Idempotency-Key"] = key
    req = urllib.request.Request(url, data=body, headers=headers, method=method)
    start = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            raw = response.read()
            return {"status": response.status, "body": raw, "headers": response.headers, "elapsed": time.perf_counter() - start, "network": None}
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        return {"status": exc.code, "body": raw, "headers": exc.headers, "elapsed": time.perf_counter() - start, "network": None}
    except Exception as exc:  # network/timeouts are summarized, not hidden
        return {"status": 0, "body": b"", "headers": {}, "elapsed": time.perf_counter() - start, "network": repr(exc)}


BASE_URL = os.environ.get("BASE_URL", "").rstrip("/")
SECRET = os.environ.get("JWT_SECRET", "")
ADMIN = ""
METRICS_REASONS = ("seat_taken", "per_user_limit", "idempotent_replay")


def require_status(result, status, label):
    if result["status"] != status:
        raise RuntimeError(f"{label}: expected HTTP {status}, got {result['status']}: {result['body'][:500]!r} network={result['network']}")
    return json.loads(result["body"] or b"{}")


def create_show(prefix, count, limit=4):
    # The public creation API currently uses its default per-user limit (4).
    seats = [f"{prefix}{i:05d}" for i in range(count)]
    result = call("POST", "/shows", ADMIN, {"name": f"burst-{prefix}-{secrets.token_hex(4)}", "seats": seats, "price_paise": 100})
    out = require_status(result, 201, "create show")
    return out["id"], seats


def reserve(show, token, seats, key):
    return call("POST", f"/shows/{show}/reserve", token, {"seats": seats}, key)


def cancel(reservation_id, token):
    return call("POST", f"/reservations/{reservation_id}/cancel", token, {})


def error_code(result):
    try:
        return json.loads(result["body"]).get("error", {}).get("code", "unknown")
    except Exception:
        return "unknown"


def record(result, latency_list, outcomes):
    latency_list.append(result["elapsed"])
    status = result["status"]
    if result["network"]:
        outcomes["network_errors"] += 1
    elif status == 201:
        outcomes["201"] += 1
        if str(result["headers"].get("Idempotent-Replayed", "")).lower() == "true":
            outcomes["replays"] += 1
    elif status == 409:
        outcomes[f"409 {error_code(result)}"] += 1
    elif 400 <= status < 500:
        outcomes["other_4xx"] += 1
    elif status >= 500:
        outcomes["5xx"] += 1
    elif 200 <= status < 300:
        outcomes["other_2xx"] += 1
    else:
        outcomes[f"other_{status}"] += 1


def parallel(jobs, workers=128):
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
        return list(pool.map(lambda job: job(), jobs))


def scrape_metrics():
    response = call("GET", "/metrics")
    if response["status"] != 200:
        raise RuntimeError(f"metrics scrape: expected HTTP 200, got {response['status']}: {response['body'][:500]!r}")
    text = response["body"].decode()
    values = {"confirmed": 0, **{reason: 0 for reason in METRICS_REASONS}}
    for line in text.splitlines():
        if line.startswith("reservations_confirmed_total "):
            values["confirmed"] = int(float(line.split()[-1]))
        elif line.startswith("reservations_declined_total{"):
            for reason in METRICS_REASONS:
                if f'reason="{reason}"' in line:
                    values[reason] = int(float(line.split()[-1]))
    return values


def reconcile(show_id, expected):
    result = call("GET", f"/shows/{show_id}")
    body = require_status(result, 200, f"reconcile {show_id}")
    seats = body.get("seats", [])
    counts = Counter(seat["status"] for seat in seats)
    total = body["total_seats"]
    if len(seats) != total or sum(counts[s] for s in ("available", "held", "confirmed")) != total:
        raise RuntimeError(f"invariant violation show={show_id}: counts={counts}, len={len(seats)}, total={total}")
    actual = {seat["seat_id"]: seat["status"] for seat in seats}
    mismatches = {seat: (wanted, actual.get(seat)) for seat, wanted in expected.items() if actual.get(seat) != wanted}
    if mismatches:
        raise RuntimeError(f"client/server seat-state mismatch show={show_id}: {mismatches}")
    return {"show_id": show_id, "available": body["available"], "held": body["held"], "confirmed": body["confirmed"], "total_seats": total, "listed_seats": len(seats)}


def run():
    if not BASE_URL or not SECRET:
        raise RuntimeError("Set BASE_URL and JWT_SECRET, e.g. make burst BASE_URL=http://localhost:8080 JWT_SECRET=...")
    global ADMIN
    ADMIN = jwt(SECRET, "burst-admin", "admin")
    nonce = secrets.token_hex(5)
    users = {}

    def token(user):
        if user not in users:
            users[user] = jwt(SECRET, user, "user")
        return users[user]

    latency = []
    outcomes = Counter()
    expected_metrics = Counter()
    shows = []
    details = []

    # Create the isolated datasets before taking counter baselines.
    hot, hot_seats = create_show("H", 1); shows.append(hot)
    stamp, stamp_seats = create_show("S", 100); shows.append(stamp)
    limit_show, limit_seats = create_show("L", 10); shows.append(limit_show)
    idem_show, idem_seats = create_show("I", 3); shows.append(idem_show)
    churn_show, churn_seats = create_show("C", 2); shows.append(churn_show)
    before = scrape_metrics()
    workload_start = time.perf_counter()

    # 500 distinct JWT identities contend for one hot seat.
    hot_results = parallel([lambda i=i: reserve(hot, token(f"{nonce}-hot-{i}"), [hot_seats[0]], f"{nonce}-hot-{i}") for i in range(500)])
    for result in hot_results:
        record(result, latency, outcomes)
    winners = [r for r in hot_results if r["status"] == 201]
    if len(winners) != 1:
        raise RuntimeError(f"hot-seat storm produced {len(winners)} winners; required exactly one")
    expected_metrics["confirmed"] += 1
    expected_metrics["seat_taken"] += 499
    hot_body = json.loads(winners[0]["body"])
    hot_expected = {hot_seats[0]: "confirmed"}
    details.append({"scenario": "hot-seat storm", "requests": 500, "201": 1, "409 seat_taken": 499})

    # 1,000 unique requests skew across 100 seats, followed by enough concurrent
    # same-key retries to make this a 20,000-request stampede in total.
    primary_jobs = []
    primary_meta = []
    for i in range(1000):
        user = f"{nonce}-stamp-{i}"
        seat = stamp_seats[i % len(stamp_seats)]
        key = f"{nonce}-stamp-key-{i}"
        primary_jobs.append(lambda user=user, seat=seat, key=key: reserve(stamp, token(user), [seat], key))
        primary_meta.append((user, seat, key))
    primary = parallel(primary_jobs, workers=128)
    for result in primary:
        record(result, latency, outcomes)
    winners_meta = []
    stamp_expected = {}
    stamp_taken = 0
    for meta, result in zip(primary_meta, primary):
        if result["status"] == 201:
            winners_meta.append((meta, json.loads(result["body"])))
            stamp_expected[meta[1]] = "confirmed"
        elif result["status"] == 409 and error_code(result) == "seat_taken":
            stamp_taken += 1
        else:
            raise RuntimeError(f"unexpected primary stampede outcome: {result['status']} {result['body'][:300]!r}")
    expected_metrics["confirmed"] += len(winners_meta)
    expected_metrics["seat_taken"] += stamp_taken
    replay_count = 20000 - len(primary)
    retry_jobs = []
    for i in range(replay_count):
        (user, seat, key), _ = winners_meta[i % len(winners_meta)]
        retry_jobs.append(lambda user=user, seat=seat, key=key: reserve(stamp, token(user), [seat], key))
    retries = parallel(retry_jobs, workers=128)
    for result in retries:
        record(result, latency, outcomes)
    bad_replays = [r for r in retries if r["status"] != 201 or str(r["headers"].get("Idempotent-Replayed", "")).lower() != "true"]
    if bad_replays:
        raise RuntimeError(f"stampede retry did not replay: {bad_replays[0]['status']} {bad_replays[0]['body'][:300]!r}")
    expected_metrics["idempotent_replay"] += replay_count
    details.append({"scenario": "skewed 20k-request stampede", "requests": len(primary) + len(retries), "unique_requests": len(primary), "successful_keys": len(winners_meta), "409 seat_taken": stamp_taken, "same-key replays": replay_count})

    # Ten distinct seat requests from one user race against a per-user limit of four.
    limit_results = parallel([lambda i=i: reserve(limit_show, token(f"{nonce}-limit-user"), [limit_seats[i]], f"{nonce}-limit-{i}") for i in range(10)])
    for result in limit_results:
        record(result, latency, outcomes)
    limit_wins = [r for r in limit_results if r["status"] == 201]
    limit_denials = [r for r in limit_results if r["status"] == 409 and error_code(r) == "per_user_limit_exceeded"]
    if len(limit_wins) != 4 or len(limit_denials) != 6:
        raise RuntimeError(f"per-user limit mismatch: 201={len(limit_wins)}, limit 409={len(limit_denials)}")
    expected_metrics["confirmed"] += 4
    expected_metrics["per_user_limit"] += 6
    limit_expected = {limit_seats[i]: "available" for i in range(10)}
    for r in limit_wins:
        for seat in json.loads(r["body"])["seats"]:
            limit_expected[seat] = "confirmed"
    details.append({"scenario": "per-user limit", "requests": 10, "201": 4, "409 per_user_limit_exceeded": 6})

    # Same key replay, then same key with a different body must conflict.
    first = reserve(idem_show, token(f"{nonce}-idem"), [idem_seats[0]], f"{nonce}-same-key")
    record(first, latency, outcomes)
    require_status(first, 201, "initial idempotent reserve")
    expected_metrics["confirmed"] += 1
    repeats = parallel([lambda: reserve(idem_show, token(f"{nonce}-idem"), [idem_seats[0]], f"{nonce}-same-key") for _ in range(50)])
    for result in repeats:
        record(result, latency, outcomes)
    if any(r["status"] != 201 or r["body"] != first["body"] or str(r["headers"].get("Idempotent-Replayed", "")).lower() != "true" for r in repeats):
        raise RuntimeError("same-key same-body concurrent replay mismatch")
    expected_metrics["idempotent_replay"] += len(repeats)
    different = reserve(idem_show, token(f"{nonce}-idem"), [idem_seats[1]], f"{nonce}-same-key")
    record(different, latency, outcomes)
    if different["status"] != 409 or error_code(different) != "idempotency_key_conflict":
        raise RuntimeError(f"same-key different-body did not return idempotency conflict: {different['status']} {different['body']!r}")
    idem_expected = {seat: "available" for seat in idem_seats}; idem_expected[idem_seats[0]] = "confirmed"
    details.append({"scenario": "same key", "requests": 52, "201": 51, "409 idempotency_key_conflict": 1, "replays": 50})

    # Repeated reserve/cancel/rebook races on the same two seats.
    churn_expected = {seat: "available" for seat in churn_seats}
    churn_cancel_count = 0
    for i in range(50):
        owner = f"{nonce}-churn-{i}"
        response = reserve(churn_show, token(owner), [churn_seats[i % 2]], f"{nonce}-churn-key-{i}")
        record(response, latency, outcomes)
        require_status(response, 201, "churn reserve")
        expected_metrics["confirmed"] += 1
        body = json.loads(response["body"])
        cancelled = cancel(body["reservation_id"], token(owner))
        if cancelled["status"] != 200:
            raise RuntimeError(f"churn cancel got {cancelled['status']}: {cancelled['body']!r}")
        record(cancelled, latency, outcomes)
        churn_cancel_count += 1
        churn_expected[churn_seats[i % 2]] = "available"
        other = f"{nonce}-churn-rebook-{i}"
        rebook = reserve(churn_show, token(other), [churn_seats[i % 2]], f"{nonce}-churn-rebook-key-{i}")
        record(rebook, latency, outcomes)
        require_status(rebook, 201, "churn rebook")
        expected_metrics["confirmed"] += 1
        churn_expected[churn_seats[i % 2]] = "confirmed"
        # Release the rebooked seat so next iteration can reuse it.
        rebook_body = json.loads(rebook["body"])
        release = cancel(rebook_body["reservation_id"], token(other))
        if release["status"] != 200:
            raise RuntimeError(f"churn rebook cancel got {release['status']}: {release['body']!r}")
        record(release, latency, outcomes)
        churn_cancel_count += 1
        churn_expected[churn_seats[i % 2]] = "available"
    details.append({"scenario": "cancel/rebook churn", "cycles": 50, "reserve/rebook 201": 100, "cancels": churn_cancel_count})

    workload_end = time.perf_counter()
    final = [reconcile(show_id, expected) for show_id, expected in [(hot, hot_expected), (stamp, stamp_expected), (limit_show, limit_expected), (idem_show, idem_expected), (churn_show, churn_expected)]]
    after = scrape_metrics()
    actual_delta = {"confirmed": after["confirmed"] - before["confirmed"], **{reason: after[reason] - before[reason] for reason in METRICS_REASONS}}
    wanted_delta = {"confirmed": expected_metrics["confirmed"], **{reason: expected_metrics[reason] for reason in METRICS_REASONS}}
    if actual_delta != wanted_delta:
        raise RuntimeError(f"reservation metric delta mismatch: expected={wanted_delta}, actual={actual_delta}")

    if outcomes["5xx"] or outcomes["network_errors"]:
        raise RuntimeError(f"load had 5xx/network errors: 5xx={outcomes['5xx']} network={outcomes['network_errors']}")
    unknown = {key: val for key, val in outcomes.items() if key not in {"201", "409 seat_taken", "409 per_user_limit_exceeded", "409 idempotency_key_conflict", "other_2xx", "other_4xx", "5xx", "network_errors", "replays"} and val}
    if unknown:
        raise RuntimeError(f"unexpected response classes: {unknown}")

    elapsed = workload_end - workload_start
    sorted_latency = sorted(latency)
    percentile = lambda p: sorted_latency[min(len(sorted_latency) - 1, int((len(sorted_latency) - 1) * p))] * 1000
    report = {
        "created_at": datetime.now(timezone.utc).isoformat(),
        "base_url": BASE_URL,
        "requests": len(latency),
        "outcomes": dict(sorted(outcomes.items())),
        "latency_ms": {"p50": round(percentile(.50), 2), "p95": round(percentile(.95), 2), "p99": round(percentile(.99), 2)},
        "requests_per_second": round(len(latency) / elapsed, 2) if elapsed else 0,
        "reservation_metric_delta": {"expected": wanted_delta, "actual": actual_delta},
        "scenarios": details,
        "final_reconciliation": final,
    }
    print(json.dumps(report, indent=2, sort_keys=True))
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", help="also write a Markdown summary to this path")
    args = parser.parse_args()
    try:
        report = run()
        if args.out:
            lines = ["# Reservation burst summary", "", f"- Created: {report['created_at']}", f"- Base URL: {report['base_url']}", f"- Requests: {report['requests']}", f"- Requests/sec: {report['requests_per_second']}", f"- Latency ms p50/p95/p99: {report['latency_ms']['p50']} / {report['latency_ms']['p95']} / {report['latency_ms']['p99']}", "", "## Outcome distribution", "", "```json", json.dumps(report["outcomes"], indent=2, sort_keys=True), "```", "", "## Reservation metric deltas", "", "```json", json.dumps(report["reservation_metric_delta"], indent=2, sort_keys=True), "```", "", "## Scenarios", ""]
            lines += [f"- {item['scenario']}: `{json.dumps(item, sort_keys=True)}`" for item in report["scenarios"]]
            lines += ["", "## Final reconciliation", "", "```json", json.dumps(report["final_reconciliation"], indent=2, sort_keys=True), "```", ""]
            os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
            with open(args.out, "w", encoding="utf-8") as stream:
                stream.write("\n".join(lines))
            print(f"Summary saved to {args.out}")
    except Exception as exc:
        print(f"BURST FAIL: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
