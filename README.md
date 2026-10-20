# Fridge

Creates Postgres nodes behind one Traefik port (routed by TLS SNI), backs them up to S3 and restores them.

## Run it (docker compose)

```sh
# 1. secrets (deploy/.env is gitignored; every variable is explained in deploy/.env.example)
cp deploy/.env.example deploy/.env
# then fill FRIDGE_SECRET_KEY (openssl rand -base64 32), FRIDGE_TOKEN (openssl rand -hex 32),
# FRIDGE_META_PASSWORD (openssl rand -hex 16); optional FRIDGE_ALERT_WEBHOOK, TZ

# 2. start (add --profile dev for a local MinIO with bucket "fridge", minioadmin/minioadmin)
docker compose -f deploy/docker-compose.yml --profile dev up -d --build

# 3. CLI (talks to the API through Traefik on :8080)
go build -o fridge ./cmd/fridge
export FRIDGE_URL=http://localhost:8080 FRIDGE_TOKEN=<token from deploy/.env>
./fridge dest add local --endpoint http://minio:9000 --bucket fridge \
  --access-key minioadmin --secret-key minioadmin --path-style
./fridge dest test local
./fridge create postgres abc --domain abc.com --version 16   # prints the connection string
./fridge backup abc --wait
./fridge objects local --prefix fridge/postgres-16
./fridge restore --from local:<key> --name abc2 --domain abc2.com --version 17 --wait
```

Clients connect on port 5432 with TLS; the hostname selects the database (SNI). Without DNS, let `host` drive SNI
and `hostaddr` pick the server:

```sh
psql "postgresql://abc_app:<pw>@abc.com:5432/abc?sslmode=require&hostaddr=127.0.0.1"
./fridge ca > fridge-ca.crt   # then sslmode=verify-full&sslrootcert=fridge-ca.crt
```

The IP allowlist (default `10.0.0.0/8 172.16.0.0/12 192.168.0.0/16`) also guards the API behind Traefik. If you lock
yourself out, set it from inside the container, which bypasses Traefik:

```sh
docker compose -f deploy/docker-compose.yml exec fridge /fridge allowlist set 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16
```

Stopping `fridge` does not interrupt database traffic; Traefik keeps routing with the last config.

## `fridge server` environment

| Variable | Default | |
|---|---|---|
| `FRIDGE_DATABASE_URL` | — (required) | metadata Postgres; compose points it at `fridge-meta` |
| `FRIDGE_SECRET_KEY` | — (required) | base64 of exactly 32 bytes; encrypts DB passwords and S3 secrets — keep it, losing it loses them |
| `FRIDGE_TOKEN` | — (required) | bearer token for the API / CLI |
| `FRIDGE_LISTEN` | `:8080` | HTTP listen address |
| `FRIDGE_DATA` | `/data` | holds the internal CA (`<data>/ca`) |
| `FRIDGE_TRAEFIK_DIR` | `/traefik` | shared with Traefik: `dynamic/` (routes), `certs/` |
| `FRIDGE_NETWORK` | `fridge-net` | Docker network for database containers |
| `FRIDGE_API_BACKEND` | `http://fridge:8080` | URL Traefik uses to reach the API |
| `FRIDGE_ALERT_WEBHOOK` | empty (log only) | receives `POST {"text": "..."}` on failed backups/restores |
| `TZ` | `UTC` | time zone the backup schedules' cron expressions run in (e.g. `Asia/Ho_Chi_Minh`); compose passes it through from `deploy/.env` |

Compose also reads `FRIDGE_META_PASSWORD` (use URL-safe characters, it goes into `FRIDGE_DATABASE_URL`).
`fridge healthcheck` exits 0 when `/readyz` answers 200 (used by the compose healthcheck).

## Tests

```sh
bash scripts/test-db.sh && go test ./...
# end-to-end (Docker + MinIO): backup a Postgres 14 node, restore it into Postgres 17
FRIDGE_IT_S3_ENDPOINT=http://localhost:59000 FRIDGE_IT_S3_ACCESS=minioadmin FRIDGE_IT_S3_SECRET=minioadmin \
FRIDGE_IT_S3_BUCKET=fridge-it go test -tags integration -timeout 20m ./internal/app/ -run E2E -v
```
