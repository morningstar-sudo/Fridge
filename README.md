# Fridge

Creates Postgres nodes on Docker Swarm, reachable on one port (routed by TLS SNI), backs them up to S3 and restores
them. A web UI and a CLI, with user accounts and per-user ownership, drive it.

Everything is a swarm service. Adding a server is `docker swarm join`: nothing else is installed on it.

## Install

On the server that becomes the swarm manager (needs Docker):

```sh
curl -fsSL https://raw.githubusercontent.com/morningstar-sudo/Fridge/main/install.sh | sh -s -- --admin alice
```

On Windows, `irm https://raw.githubusercontent.com/morningstar-sudo/Fridge/main/install.ps1 | iex` does the same inside
WSL. The script starts a swarm if there is none, labels the server, writes `.env` (secrets are generated only when
missing; `FRIDGE_SECRET_KEY` is never regenerated), deploys the stack, waits until it answers and prints the web UI
address and the admin password (once). Run it again to upgrade; `--version TAG` picks an image tag, `--hosts a,b` names
the hosts the web certificate must cover. `--uninstall` stops Fridge and leaves databases and data;
`--uninstall --purge` deletes everything. The images are `ghcr.io/morningstar-sudo/fridge` and `…/fridge-job`
(make the packages public, or `docker login ghcr.io` first).

By hand: copy `deploy/env.example` to `.env` next to `deploy/stack.yml`, fill it in, then
`set -a; . ./.env; set +a; docker stack deploy --with-registry-auth -c stack.yml fridge`.

The web UI and API are on **https://\<server\>:8443**. The certificate is signed by Fridge's own CA; to avoid the browser
warning, download the CA from `https://<server>:8443/api/ca` (or `fridge ca`) and add it to your trust store. The first
visit shows the Setup page when no admin exists: enter `FRIDGE_SETUP_TOKEN` from `.env` and choose a name and password.
The password never leaves the browser (SCRAM-SHA-256 login).

## How it is laid out

| Service | Where | What |
|---|---|---|
| `traefik` | every server (global) | database port 5432 on the swarm routing mesh, web/API port 8443 on the server itself; routes and certificates are fetched from Fridge, nothing is stored |
| `fridge` | the server labelled `fridge.control=true` | the API, the scheduler, the swarm client (needs a manager) |
| `fridge-meta` | same server | metadata Postgres |
| `fridge-db-<key>` | one server per database (`fridge.db=true`), pinned, with a local volume | the database |
| `fridge-edge-<key>` | next to its database | joins the shared edge network and the database network; relays to its database over TLS with that database's client certificate |

All databases share one internal, encrypted overlay (`fridge_db`: no internet, nothing else on it). What keeps one
database away from another is the database itself: its `pg_hba.conf` accepts only a TLS client presenting that
database's own certificate (`hostssl … scram-sha-256 clientcert=verify-ca`, the certificate being the only trusted
root) and then the password, and rejects everything else, so a compromised database can reach the port of another but
not get past the TLS handshake. Only that database's edge sidecar and the jobs working on it are given its client key.
The Fridge API and the metadata database are not on `fridge_db` at all. The database presents one certificate, naming
both its public domain and its service: it is the same certificate Traefik presents to clients, which is what lets
SCRAM channel binding (libpq's default) work through the proxy. Node certificates last five years; Fridge alerts 60
days before one expires. Dump, restore and the deep check of a backup run as one-shot services
(`fridge-job` image, PostgreSQL clients 13–18) that join `fridge_db` and the empty `fridge_egress` network (the check
job only the latter) and talk to S3 themselves; nothing is streamed through Fridge.

**Add a server:** `docker swarm join …` on it, then on the manager `docker node update --label-add fridge.db=true <node>`
so it may hold databases. New databases go to the labelled server holding the fewest. A database lives on one server with
its local volume; if that server goes down the database is down until it returns (restore from S3 to move it).

**Firewall.** Open 8443 (web UI/API) and 5432 (databases). The routing mesh hides client addresses, so Fridge cannot
filter port 5432 by IP: restrict it with the host firewall or the cloud security group. The IP allowlist (Settings, default
loopback and the private ranges) applies to the web UI and API, whose port is published on each server itself.

## Using it

Each user owns the databases and S3 destinations they create; admins see and manage everything. Database and role names
carry the owner's account name as a prefix (`alice_shop`). User-created destinations may not point into Fridge's own
networks or at loopback/link-local addresses (an admin can exempt hosts under Settings). Backup files are checked before
a restore (header) and, on request, by a real `pg_restore` in a job that has S3 and nothing else.

### CLI

```sh
go build -o fridge ./cmd/fridge
export FRIDGE_URL=https://<server>:8443       # default https://localhost:8443
./fridge login --user alice                   # first time: shows the CA fingerprint and asks before trusting it
./fridge domains add shop.example.com         # a database needs a domain you registered
./fridge dest add local --endpoint https://s3.example.com --bucket fridge --access-key AK --secret-key SK
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

If nobody can log in any more, three commands work on the metadata database from inside the server container
(`docker exec $(docker ps -q -f name=fridge_fridge) /fridge recover …`):

```sh
/fridge recover allowlist 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16
/fridge recover reset-password alice   # prints a one-time link
/fridge recover promote alice
```

Clients connect on port 5432 with TLS; the hostname selects the database (SNI), on any server of the swarm. Without DNS,
let `host` drive SNI and `hostaddr` pick the server:

```sh
psql "postgresql://abc_app:<pw>@abc.com:5432/abc?sslmode=require&hostaddr=203.0.113.10"
./fridge ca --db alice_abc > abc-ca.crt   # then sslmode=verify-full&sslrootcert=abc-ca.crt
```

Plain `sslmode=require` encrypts but does not check who answers. To check, trust the database's CA: **every account has a
CA of its own**, created the first time they get a database, and signs the certificates of their databases only. It is
downloaded from the database's page in the web UI ("tải CA của database này"), with `fridge ca --db <name>`, or from
`GET /api/databases/<name>/ca`; everyone who may see a database (members, admins) gets its owner's CA. A client that trusts
it trusts that owner's databases and no one else's, and the instance CA (the web UI's, `fridge ca`) vouches for none of
them. Databases made before tenant CAs existed keep their instance-CA certificate until recreated. The CA's private key
never leaves the server (stored encrypted).

Stopping `fridge` does not interrupt database traffic; Traefik keeps routing with the last config it fetched. A Traefik
that starts while Fridge is down has no routes until Fridge is back. Fridge checks the swarm at boot and every five
minutes: it recreates a missing edge sidecar, cleans up what failed creations left behind, and alerts (once per problem)
about a database service that is gone or swarm resources that belong to no database.

The Images page lists the catalog images, pulls one on every server (a job pinned to each, no agent) and reports newer
versions in the registry. There is no Prune: nothing runs on the other servers to clean them.

## `fridge server` environment

| Variable | Default | |
|---|---|---|
| `FRIDGE_DATABASE_URL` | — (required) | metadata Postgres |
| `FRIDGE_SECRET_KEY` | — (required) | base64 of exactly 32 bytes; encrypts DB passwords and S3 secrets — keep it, losing it loses them |
| `FRIDGE_PROVIDER_TOKEN` | — (required) | bearer token of the Traefik configuration endpoint |
| `FRIDGE_SERVER_IMAGE` | — (required) | the server image; also runs the edge sidecars and volume jobs |
| `FRIDGE_JOB_IMAGE` | — (required) | the dump/restore/check job image |
| `FRIDGE_SETUP_TOKEN` | empty | lets the first admin be created; unused once a user exists |
| `FRIDGE_PUBLIC_HOSTS` | empty | comma-separated host names/IPs for the web certificate (`localhost`, `127.0.0.1` are always included); reissued when this changes, renewed 30 days before expiry |
| `FRIDGE_TRUSTED_PROXY` | the edge network's subnet | CIDRs of peers whose `X-Forwarded-For` is believed |
| `FRIDGE_LISTEN` | `:8080` | plain-HTTP API address inside the container (Traefik terminates TLS in front of it) |
| `FRIDGE_PROVIDER_LISTEN` | `:8081` | Traefik's configuration endpoint (edge network only, token required) |
| `FRIDGE_DATA` | `/data` | holds the internal CA (`<data>/ca`) and the web certificate (`<data>/web`) |
| `FRIDGE_EDGE_NETWORK` / `FRIDGE_EGRESS_NETWORK` / `FRIDGE_DB_NETWORK` | `fridge_edge` / `fridge_egress` / `fridge_db` | shared overlays: Traefik, Fridge and sidecars / empty and open to the internet, for jobs / every database (internal) |
| `FRIDGE_API_BACKEND` | `http://fridge:8080` | URL Traefik uses to reach the API |
| `FRIDGE_ALERT_WEBHOOK` | empty (log only) | receives `POST {"text": "..."}` on failed backups/restores and swarm problems |
| `TZ` | `UTC` | time zone the backup schedules' cron expressions run in |

Passwords reach services and jobs through their environment (as before; Docker secrets are a later step).
`fridge healthcheck` exits 0 when `/readyz` answers 200.

## Tests

```sh
bash scripts/test-db.sh && go test ./...          # unit tests; the metadata-database tests need Postgres 18 from this script

# everything below runs in a throwaway two-node swarm made of two docker:dind containers, never on your own Docker
docker build -f deploy/Dockerfile -t fridge:dev . && docker build -f deploy/Dockerfile --target job -t fridge-job:dev .
IMAGES="fridge:dev fridge-job:dev postgres:17 postgres:18 traefik:v3.5 cgr.dev/chainguard/minio:latest cgr.dev/chainguard/minio-client:latest"
bash scripts/swarm-test/up.sh $IMAGES
bash scripts/swarm-test/gotest.sh        # swarm client, jobs, isolation (internal/app) and users/ownership through the API (internal/api)
bash scripts/swarm-test/down.sh && bash scripts/swarm-test/up.sh $IMAGES
bash scripts/swarm-test/e2e.sh           # install.sh, mesh access from both servers, TLS/SNI refusals, Traefik restart, drain, reconcile
bash scripts/swarm-test/down.sh && bash scripts/swarm-test/up.sh $IMAGES
bash scripts/ui-smoke/run.sh             # the whole web UI in a real browser (Playwright in Docker) against the installed stack
bash scripts/swarm-test/down.sh
```

`.github/workflows/ci.yml` runs the same, and publishes the two images (`linux/amd64`, `linux/arm64`) from `main` and
`v*` tags.
