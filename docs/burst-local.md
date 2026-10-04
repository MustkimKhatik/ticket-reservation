# Reservation burst summary

- Created: 2026-10-04T11:28:25.073428+00:00
- Base URL: http://localhost:8080
- Requests: 20762
- Requests/sec: 1643.1
- Latency ms p50/p95/p99: 62.22 / 116.17 / 149.39

## Outcome distribution

```json
{
  "201": 19256,
  "409 idempotency_key_conflict": 1,
  "409 per_user_limit_exceeded": 6,
  "409 seat_taken": 1399,
  "other_2xx": 100,
  "replays": 19050
}
```

## Reservation metric deltas

```json
{
  "actual": {
    "confirmed": 206,
    "idempotent_replay": 19050,
    "per_user_limit": 6,
    "seat_taken": 1399
  },
  "expected": {
    "confirmed": 206,
    "idempotent_replay": 19050,
    "per_user_limit": 6,
    "seat_taken": 1399
  }
}
```

## Scenarios

- hot-seat storm: `{"201": 1, "409 seat_taken": 499, "requests": 500, "scenario": "hot-seat storm"}`
- skewed request stampede: `{"409 seat_taken": 900, "requests": 20000, "same-key replays": 19000, "scenario": "skewed request stampede", "successful_keys": 100, "unique_requests": 1000}`
- per-user limit: `{"201": 4, "409 per_user_limit_exceeded": 6, "requests": 10, "scenario": "per-user limit"}`
- same key: `{"201": 51, "409 idempotency_key_conflict": 1, "replays": 50, "requests": 52, "scenario": "same key"}`
- cancel/rebook churn: `{"cancels": 100, "cycles": 50, "reserve/rebook 201": 100, "scenario": "cancel/rebook churn"}`

## Final reconciliation

```json
[
  {
    "available": 0,
    "confirmed": 1,
    "held": 0,
    "listed_seats": 1,
    "show_id": "4d7fc95b-5bfa-4892-8b86-3e3efce2ff08",
    "total_seats": 1
  },
  {
    "available": 0,
    "confirmed": 100,
    "held": 0,
    "listed_seats": 100,
    "show_id": "55fe0cf2-ce0b-4fa5-8ebd-133960ef0cbc",
    "total_seats": 100
  },
  {
    "available": 6,
    "confirmed": 4,
    "held": 0,
    "listed_seats": 10,
    "show_id": "e7806d76-8d08-48a2-8b9d-49f213fe4058",
    "total_seats": 10
  },
  {
    "available": 2,
    "confirmed": 1,
    "held": 0,
    "listed_seats": 3,
    "show_id": "852d7a01-9a13-4aa2-bdd2-4a4205e9869f",
    "total_seats": 3
  },
  {
    "available": 2,
    "confirmed": 0,
    "held": 0,
    "listed_seats": 2,
    "show_id": "75305378-5d90-4be2-8ab1-db1f3772d2d5",
    "total_seats": 2
  }
]
```
