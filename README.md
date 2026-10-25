# Fridge

Creates Postgres nodes behind one Traefik port (routed by TLS SNI), backs them up to S3 and restores them.
A web UI and a CLI, with user accounts and per-user ownership, drive it.

## Run it (docker compose)

```sh
# 1. secrets (deploy/.env is gitignored; every variable is explained in deploy/.env.example)
cp deploy/.env.example deploy/.env
# then fill FRIDGE_META_PASSWORD (openssl rand -hex 16), FRIDGE_SECRET_KEY (openssl rand -base64 32),
# FRIDGE_SETUP_TOKEN (openssl rand -hex 32) and FRIDGE_PUBLIC_HOSTS (the names/IPs you reach the UI by,
# e.g. fridge.lan,192.168.1.10); optional FRIDGE_ALERT_WEBHOOK, TZ

# 2. start (add --profile dev for a local MinIO with bucket "fridge", minioadmin/minioadmin)
docker compose -f deploy/docker-compose.yml --profile dev up -d --build
```

The web UI and the API are on **https://localhost:8443** (plain `:8080` only redirects there). The certificate is
signed by Fridge's own CA; to avoid the browser warning, download the CA from `https://localhost:8443/api/ca`
(or `fridge ca`) and add it to your trust store. The first visit shows the Setup page: enter `FRIDGE_SETUP_TOKEN` and
choose the admin account's name and password. The password never leaves the browser (SCRAM-SHA-256 login).
The setup token is not used again once a user exists.

**Upgrading from the Postgres 17 metadata database:** `fridge-meta` now runs `postgres:18`, which cannot open a
volume made by 17. Development/smoke data: `docker compose -f deploy/docker-compose.yml down` then
`docker volume rm fridge_fridge-meta`. Real data must be dumped from the old database and restored into the new one.

## Using it

Each user owns the databases and S3 destinations they create; admins see and manage everything. Database and role
names carry the owner's account name as a prefix (`alice_shop`). Every database node runs on its own internal Docker
network that only Traefik joins, so one node cannot reach another node, the metadata database or the Fridge API.
User-created destinations may not point at Fridge's own networks or at loopback/link-local addresses (an admin can
exempt hosts under Settings). Backup files are checked before a restore (header) and, on request, by a real
`pg_restore` in a throwaway container without network.

### CLI

```sh
go build -o fridge ./cmd/fridge
export FRIDGE_URL=https://localhost:8443      # the default
./fridge login --user alice                   # first time: shows the CA fingerprint and asks before trusting it
./fridge domains add shop.example.com         # a database needs a domain you registered
./fridge dest add local --endpoint http://minio:9000 --bucket fridge --access-key minioadmin --secret-key minioadmin --path-style
./fridge create postgres shop --domain shop.example.com --version 16 --random-credentials   # creates alice_shop; prints the generated user and password once
./fridge show alice_shop --reveal             # connection string with the password (audited)
./fridge backup alice_shop --wait
./fridge check --from local:<key> --wait      # deep check of a backup file with a real pg_restore
./fridge restore --from local:<key> --name copy --domain copy.example.com --version 17 --wait
./fridge members add alice_shop bob           # bob can back up and read the connection string, not add people
./fridge audit --since 24h
```

`fridge login` saves the session in `~/.config/fridge/session` (0600) and the CA in `~/.config/fridge/ca.pem`; a saved
session is only ever sent to the server it came from, and there is no way to switch certificate checking off.
`FRIDGE_SESSION` overrides the session file, `FRIDGE_CA_FILE` names a CA to trust. A server with no browser at hand can
create its first admin with `fridge setup --setup-token T --user NAME`.

If nobody can log in any more, three commands work on the metadata database from inside the server container:

```sh
docker compose -f deploy/docker-compose.yml exec fridge /fridge recover allowlist 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16
docker compose -f deploy/docker-compose.yml exec fridge /fridge recover reset-password alice   # prints a one-time link
docker compose -f deploy/docker-compose.yml exec fridge /fridge recover promote alice
```

Clients connect on port 5432 with TLS; the hostname selects the database (SNI). Without DNS, let `host` drive SNI
and `hostaddr` pick the server:

```sh
psql "postgresql://abc_app:<pw>@abc.com:5432/abc?sslmode=require&hostaddr=127.0.0.1"
./fridge ca > fridge-ca.crt   # then sslmode=verify-full&sslrootcert=fridge-ca.crt
```

The IP allowlist (default `10.0.0.0/8 172.16.0.0/12 192.168.0.0/16`) also guards the web UI and API behind Traefik.
Admins change it under Settings or with `fridge allowlist set <cidr>...`; if that locks you out, use
`recover allowlist` above, which bypasses Traefik.

Stopping `fridge` does not interrupt database traffic; Traefik keeps routing with the last config.

## `fridge server` environment

| Variable | Default | |
|---|---|---|
| `FRIDGE_DATABASE_URL` | — (required) | metadata Postgres; compose points it at `fridge-meta` |
| `FRIDGE_SECRET_KEY` | — (required) | base64 of exactly 32 bytes; encrypts DB passwords and S3 secrets — keep it, losing it loses them |
| `FRIDGE_SETUP_TOKEN` | empty | lets the first admin be created; without it setup is impossible until it is set. Unused once a user exists |
| `FRIDGE_PUBLIC_HOSTS` | empty | comma-separated host names/IPs for the web certificate (`localhost`, `127.0.0.1` are always included); the certificate is reissued when this changes and renewed 30 days before it expires |
| `FRIDGE_TRUSTED_PROXY` | the `fridge-net` subnet | CIDRs of peers whose `X-Forwarded-For` is believed; anything else is taken at its socket address |
| `FRIDGE_LISTEN` | `:8080` | plain-HTTP listen address inside the container (not published; Traefik terminates TLS in front of it) |
| `FRIDGE_DATA` | `/data` | holds the internal CA (`<data>/ca`) |
| `FRIDGE_TRAEFIK_DIR` | `/traefik` | shared with Traefik: `dynamic/` (routes), `certs/` (per-database certificates and `web.crt`/`web.key`) |
| `FRIDGE_NETWORK` | `fridge-net` | control network (traefik, fridge, fridge-meta); each DB node gets its own internal `fridge-node-<key>` network |
| `FRIDGE_TRAEFIK_CONTAINER` | `fridge-traefik` | Traefik container Fridge attaches to every node network |
| `FRIDGE_API_BACKEND` | `http://fridge:8080` | URL Traefik uses to reach the API |
| `FRIDGE_ALERT_WEBHOOK` | empty (log only) | receives `POST {"text": "..."}` on failed backups/restores |
| `TZ` | `UTC` | time zone the backup schedules' cron expressions run in (e.g. `Asia/Ho_Chi_Minh`); compose passes it through from `deploy/.env` |

Compose also reads `FRIDGE_META_PASSWORD` (use URL-safe characters, it goes into `FRIDGE_DATABASE_URL`).
`fridge healthcheck` exits 0 when `/readyz` answers 200 (used by the compose healthcheck).

Runbook: a recreated Traefik container loses its attachments to the `fridge-node-*` networks. Compose restarts Fridge when it recreates Traefik, and Fridge re-attaches them at boot and re-checks every minute. If Traefik was recreated outside compose while Fridge was down, DB routes recover within 1 minute of Fridge starting.

## Tests

```sh
bash scripts/test-db.sh && go test ./...          # the metadata-database tests need Postgres 18 from this script

# end-to-end against real Docker and an S3 (the dev profile's MinIO is enough)
S3="FRIDGE_IT_S3_ENDPOINT=http://localhost:59000 FRIDGE_IT_S3_ACCESS=minioadmin FRIDGE_IT_S3_SECRET=minioadmin FRIDGE_IT_S3_BUCKET=fridge-it"
env $S3 go test -tags integration -timeout 30m ./internal/app/ -run 'E2E|DumpCheckReal' -v   # backup a 14 node, restore into 17; real pg_restore check
env $S3 go test -tags integration -timeout 30m ./internal/api/ -run E2E -v                    # users, ownership, permissions, per-node users, network isolation
go test -tags integration ./internal/container/ -run Images -v                                # Docker image calls

bash scripts/ui-smoke/run.sh   # the whole web UI in a real browser (Playwright in Docker) against a throwaway stack
```
