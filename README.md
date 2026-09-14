# CreatorFlow Hey Folks

A creator commerce platform: public profile page, digital products, Razorpay payments, secure file delivery, and analytics.

`Next.js` → `Go / Fiber` → `PostgreSQL` · `Redis` · `S3`

## Architecture rules

These are the constraints the codebase is built around. Breaking one of them is a design change, not a refactor.

1. **Next.js never touches the database.** It is a rendering layer that calls the Go API over HTTPS, like any other client.
2. **PostgreSQL is the only source of truth.** Redis can be flushed at any moment and the system must still be correct.
3. **Business logic lives in services, SQL lives in repositories.** Handlers parse and format; they do not decide anything.
4. **Files never pass through the API.** Uploads and downloads both use presigned S3 URLs.
5. **The frontend never confirms a payment.** Orders become paid only via a signature-verified webhook.
6. **Invariants that can be constraints are constraints.** Application code is not a reliable enforcer.

## Getting started

Requires Docker and Docker Compose. Go and Node are only needed if you want to run tests or linting outside containers.

```bash
git clone <your-repo-url> creatorflow
cd creatorflow

make init      # creates .env from .env.example
# edit .env — set JWT secrets: openssl rand -base64 48

make up        # postgres + redis + minio + api + web
make health    # should return {"status":"ok", ...}
```

Services:

| Service | URL | Notes |
|---|---|---|
| Web | http://localhost:3000 | Next.js |
| API | http://localhost:8080 | Go / Fiber |
| MinIO console | http://localhost:9001 | login with `S3_ACCESS_KEY` / `S3_SECRET_KEY` |
| PostgreSQL | localhost:5432 | `make psql` |
| Redis | localhost:6379 | `make redis-cli` |

MinIO stands in for S3 locally so the same presigned-URL code path runs in development and production. Only the endpoint changes.

### Bootstrapping the frontend

The `frontend/` directory ships with a Dockerfile but no app yet. Generate it once:

```bash
npx create-next-app@latest frontend --js --app --tailwind --eslint --src-dir=false --import-alias "@/*"
```

Then `make up` will build and run it.

## Common commands

```bash
make up            # start everything
make down          # stop, keep data
make reset         # stop and destroy volumes
make logs          # tail all services
make migrate-up    # apply migrations
make migrate-new name=create_users
make test          # go test ./... -race
make lint          # golangci-lint
```

## Health endpoints

- `GET /health` — liveness. Checks nothing but the process. A database blip must not cause the orchestrator to restart healthy containers.
- `GET /ready` — readiness. Checks PostgreSQL (hard fail) and Redis (degraded, not fail). Used by the load balancer to decide whether this instance should receive traffic.

## Environments

`development` → `staging` → `production`. Each has its own database, its own secrets, and its own Razorpay keys. `.env` is never committed; production secrets come from AWS Secrets Manager.

## Layout

```
creatorflow/
├── backend/
│   ├── cmd/api/           entrypoint, routing, graceful shutdown
│   ├── internal/
│   │   ├── config/        env loading, fail-fast validation
│   │   ├── database/      pgx pool, redis client
│   │   ├── httpx/         the one error shape this API returns
│   │   └── middleware/    request ID, auth, rate limiting
│   ├── migrations/        numbered SQL, up and down
│   └── tests/             integration and API tests
├── frontend/              Next.js App Router
├── infrastructure/        docker, terraform
└── .github/workflows/     CI
```
