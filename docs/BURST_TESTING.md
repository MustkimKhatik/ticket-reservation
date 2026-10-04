# Running the reservation burst test

The same `make burst` command can target either your **local Docker Compose app** or the **deployed app**. Keep each app URL paired with that app's own `JWT_SECRET`:

| Target | URL | Signing secret |
| --- | --- | --- |
| Local | `http://localhost:8080` | `JWT_SECRET` in the local `.env` file |
| Deployed | Railway app's public URL | `JWT_SECRET` configured on the Railway app service |

Do not use the local secret with Railway or the Railway secret with localhost. A mismatch causes `401 Unauthorized` when the script creates its test shows.

The default burst sends about 20,762 HTTP requests using a client pool of up to 128 workers. That is a 20,000-request workload, not 20,000 simultaneous connections. It exercises a 500-user hot-seat contest, a 20,000-request skewed-seat stampede with same-key retries, a 10-request per-user-limit race, idempotency conflict, and 50 cancel/rebook cycles. It creates new shows and leaves the resulting test data in the target database. The report checks status distributions, latency percentiles, throughput, reservation metric deltas, and seat reconciliation.

## Local Docker Compose app

Run these steps from the repository root in Bash. `.env.example` provides local settings; the generated secret below is saved into `.env` and used by both the app and the test script.

```bash
cd into root folder
cp -n .env.example .env
JWT_SECRET="$(openssl rand -hex 32)"
sed -i "s/^JWT_SECRET=.*/JWT_SECRET=$JWT_SECRET/" .env
docker compose --env-file .env up --build -d --force-recreate
```

Wait until the local app is ready:

```bash
until curl -fsS http://localhost:8080/readyz >/dev/null; do sleep 2; done
echo "Local app is ready"
```

Run the burst against localhost and save its report:

```bash
make burst BASE_URL=http://localhost:8080 JWT_SECRET="$JWT_SECRET" OUT=docs/burst-local.md
```

View the report and local logs:

```bash
cat docs/burst-local.md
docker compose --env-file .env logs --tail=200 app db
```

If you opened a new terminal, load the matching local secret before running the test:

```bash
cd ~/Documents/ChatGPT/ticket-reservation
JWT_SECRET="$(sed -n 's/^JWT_SECRET=//p' .env)"
make burst BASE_URL=http://localhost:8080 JWT_SECRET="$JWT_SECRET" OUT=docs/burst-local.md
```

Stop the local containers when finished:

```bash
docker compose --env-file .env stop
```

## Deployed Railway app

Use the app's public Railway domain, not the MariaDB private hostname. Read the signing secret from the Railway **app service** variables. This secret must match the value currently deployed; a locally generated `.env` secret will not authenticate against Railway.

In Bash, enter the URL and securely paste the deployed secret when prompted:

```bash
BASE_URL='https://ticket-reservation-production-e646.up.railway.app'
read -rsp 'Railway app JWT_SECRET: ' JWT_SECRET
printf '\n'
```

Check the deployed app first:

```bash
curl -i "${BASE_URL%/}/livez"
curl -i "${BASE_URL%/}/readyz"
```

When both checks are healthy, run the burst against the deployed URL:

```bash
make burst BASE_URL="${BASE_URL%/}" JWT_SECRET="$JWT_SECRET" OUT=docs/burst-railway.md
```

This creates test shows and reservations in the deployed database and sends about 20,762 requests. Run it only when that traffic and persistent test data are acceptable. The metric-delta check assumes no unrelated reservations are being processed during the run and that the before/after `/metrics` scrapes reach the same app metrics process; multiple app replicas behind a load balancer can make those deltas differ.

The summary is saved locally in `docs/burst-railway.md`. Railway runtime logs remain in the Railway app service's **Deployments / Logs** view; the burst report is not uploaded to Railway. Clear the secret from the current shell when done:

```bash
unset JWT_SECRET
```

## What a passing run means

The command exits successfully only when it observes exactly one 201 for the hot seat, no 5xx or network errors, expected reservation metric deltas, and reconciled seat totals and statuses for each test show. It reports counts for 201s, 409 error codes, other 4xx, 5xx, network errors, idempotent replays, latency p50/p95/p99, and requests per second.
