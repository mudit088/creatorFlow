# Running CreatorFlow locally

Hybrid setup: infrastructure in Docker, the Go API native on the host. Everything
below assumes you are at the repo root.

## 1. Infrastructure

```powershell
docker compose up -d postgres redis minio minio-init
docker compose ps
```

| Service | Host port | Notes |
|---|---|---|
| PostgreSQL | **5433** | not 5432 — `POSTGRES_PORT` in `.env` remaps it |
| Redis | 6379 | connected, pinged by `/ready`, not yet used for anything |
| MinIO | 9000 | console on 9001, `minioadmin` / `minioadmin` |

## 2. Migrations

The `migrate` CLI is not required — run it from its pinned image on the Compose
network, which is also what CI will do:

```bash
docker run --rm --network creatorflow_default \
  -v "M:/creatorFlow/backend/migrations:/migrations" \
  migrate/migrate:v4.18.1 \
  -path=/migrations \
  -database "postgres://creatorflow:<PASSWORD>@postgres:5432/creatorflow?sslmode=disable" \
  up
```

Note `postgres:5432` inside that command: the container talks to the database
over the Compose network, where it is still on its own port. The 5433 mapping
only applies from the host.

Swap `up` for `version` to see where you are, or `down 1` to roll one back.
Current head is **7** (`orders`, `order_items`, `payments`, `payment_webhooks`,
`entitlements`).

If you prefer the CLI on PATH:

```powershell
go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.18.1
migrate -path ./backend/migrations -database $env:DATABASE_URL up
```

## 3. The API

`.env` is not read by the process — it is loaded into the shell session first.

```powershell
cd backend
Get-Content ..\.env | Where-Object { $_ -match '^\s*[^#].*=' } | ForEach-Object {
    $name, $value = $_ -split '=', 2
    Set-Item -Path "Env:$($name.Trim())" -Value $value.Trim()
}
go run ./cmd/api
```

Check it came up, and what it decided about its configuration:

```powershell
curl.exe -s http://localhost:8080/ready
```

A `WARN razorpay is not configured` line at startup means `RAZORPAY_KEY_ID` or
`RAZORPAY_KEY_SECRET` is empty. Orders will still be created; they just will not
carry a provider order or a `checkout` block.

**Changing `.env` requires restarting the API _and_ re-running the loader
block.** The process reads its environment once at startup, and the loader block
is what puts `.env` into the session in the first place.

## 4. DBeaver

| Field | Value |
|---|---|
| Host | `localhost` |
| Port | **5433** |
| Database | `creatorflow` |
| User | `creatorflow` |
| Password | see `POSTGRES_PASSWORD` in `.env` |

DBeaver reads; migrations write. Do not hand-run DDL here — schema changes go in
a numbered migration with a matching `.down.sql`.

Useful queries while testing:

```sql
-- the full state of the most recent order
SELECT o.id, o.status, o.total_minor, o.paid_at, o.provider_order_id,
       (SELECT count(*) FROM payments p WHERE p.order_id = o.id)      AS payments,
       (SELECT count(*) FROM entitlements e WHERE e.order_id = o.id)  AS entitlements
FROM orders o ORDER BY o.created_at DESC LIMIT 5;

-- webhook deliveries, including any that were recorded but refused
SELECT provider_event_id, event_type, signature_valid, processed_at, process_error
FROM payment_webhooks ORDER BY received_at DESC LIMIT 10;

-- anything that arrived but never finished processing
SELECT * FROM payment_webhooks WHERE processed_at IS NULL;
```

## 5. Postman

Import `docs/creatorflow.postman_collection.json`. It covers all 29 requests in
the order you would exercise them, and chains state automatically: Register
stores the access token, Create product stores the product id, Create order
stores the order id and total.

Two variables to set:

- `baseUrl` — defaults to `http://localhost:8080`
- `webhookSecret` — paste `RAZORPAY_WEBHOOK_SECRET` from `.env`, or the webhook
  requests will get 401

Then run the folders top to bottom. Folder 7 (Delete file) is last on purpose:
it leaves the product published with nothing to deliver, which is exactly the
state checkout refuses with a 422.

The signed webhook request computes its own signature in a pre-request script —
`hex(HMAC-SHA256(raw body, webhookSecret))`, the same thing Razorpay does.

### Running the whole collection headlessly

```bash
npx newman@6 run docs/creatorflow.postman_collection.json \
  --global-var "webhookSecret=$(grep '^RAZORPAY_WEBHOOK_SECRET=' .env | cut -d= -f2)"
```

29 requests, no failures, ending with a paid order and a granted entitlement.
`docs/sample-1024.pdf` exists so the presigned upload step has a body of exactly
the size the API signed for — replacing it with a different file will make S3
reject the PUT, which is the point of signing the length.

Regenerate the collection after changing routes:

```bash
python docs/postman_collection.gen.py
```

## 6. Webhooks from real Razorpay

Razorpay cannot reach `localhost`, so expose the API through a tunnel:

```powershell
& "C:\Program Files (x86)\cloudflared\cloudflared.exe" tunnel --url http://localhost:8080
```

Put `https://<generated-host>/api/v1/payments/webhook` in the Razorpay dashboard
webhook settings with the same secret as `.env`, subscribed to
`payment.captured`, `payment.failed` and `order.paid`.

The hostname changes every restart, so the dashboard URL needs updating each
session. The tunnel also exposes the *whole* API publicly, including
`/auth/register` — shut it down when you are finished.

## Gotchas hit so far

- **No space after `=` in `.env`.** `KEY= value` is trimmed by the PowerShell
  loader but not by Docker Compose or a shell `source`, which read the value with
  a leading space and then fail authentication confusingly.
- **Postgres is on 5433 from the host, 5432 inside Compose.**
- PowerShell's `curl` is `Invoke-WebRequest`; use `curl.exe`.
- Editing `.env` needs `docker compose up -d --force-recreate`, not `restart`.
- `@` in `POSTGRES_PASSWORD` must be `%40` inside `DATABASE_URL`.
- Git Bash rewrites paths that look absolute when calling Windows binaries.
  Prefix with `MSYS_NO_PATHCONV=1` for things like `docker exec ... psql -f /tmp/x.sql`.
