# Swarm spike (P0) — what was checked and what it decided

Run in a two-node swarm made of two `docker:28-dind` containers (`scripts/swarm-test/up.sh`, `spike.sh`, `spike-traefik.sh`).

Confirmed:
- A database service pinned with `node.hostname==X` runs there and its local volume exists only on that node.
- An `--internal --opt encrypted` overlay per database isolates databases from each other and has no egress; an empty non-internal overlay (`fridge_egress`) gives a job internet without exposing any sidecar.
- A sidecar on `fridge_edge` + the database's network (`fridge forward`) lets other services reach the database without joining its network. `--entrypoint /fridge` overrides the image's entrypoint.
- `replicated-job` services: completion is the task state `Complete`/`Failed`; the exit code is in the task error ("non-zero exit (3)"); output is readable with `service logs` after completion. `service create` must be given `-d` or the CLI waits forever for a failing job.
- `psql sslmode=require` and `verify-full` entering through the routing mesh of one node, through Traefik on another node (config from the HTTP provider, certificate inline as PEM), the sidecar and the database on a third path: works. An unknown SNI is refused.

Found, and decided:
- **Traefik must offer ALPN `postgresql`** (`tls.options.<name>.alpnProtocols: [postgresql]` on the TCP router) or libpq 17+ clients fail the handshake with "tlsv1 alert no application protocol". This also affects the single-host setup that used `tls: {}`.
- `--network none` is refused for services, and a service with no network still has egress. The "deep check" job therefore runs on `fridge_egress` only (it needs S3) and never on a database network.
- Fridge cannot rely on `docker exec` for nodes on other hosts; provisioning goes through the sidecar with pgx, dump/restore/check run as jobs.
