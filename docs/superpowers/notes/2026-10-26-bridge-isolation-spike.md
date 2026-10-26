# Spike: `bridge link set dev <veth> isolated on` instead of one overlay per database

`scripts/swarm-test/spike-isolate.sh`, two dind nodes (kernel 6.18, iproute2 from apk), one shared unencrypted overlay,
busybox listeners as fake databases (dbA, dbB on fsw1, dbC on fsw2) and a fake sidecar sc1 on fsw1.
The flag is set inside the overlay's network namespace (`nsenter --net=/var/run/docker/netns/1-<id10>`).

Measured:

| path | nothing isolated | DB veths + vxlan0 isolated | DB veths only |
|---|---|---|---|
| dbA -> dbB (same host) | open | **blocked** | blocked |
| dbA -> dbC / dbC -> dbA (other host) | open | **blocked** | **open** |
| dbC -> dbB (other host) | open | **blocked** | **open** |
| sc1 (not isolated) -> dbA, dbB (same host) | open | open | open |
| sc1 -> dbC (other host) | open | blocked | open |

- The mechanism works as the kernel documents it: isolated ports talk only to non-isolated ones. `vxlan0` must be isolated
  too, otherwise traffic from another host arrives through it and reaches the isolated veth.
- A restarted task gets a **new veth with the flag off** (`veth1` -> `veth4`): until something sets it, the new database is
  reachable from and can reach every other database. The netns can also be recreated when a host has no task on the network.
- Anything not isolated (sidecars, jobs) reaches every database on its own host. A restore job running a user's plain-SQL file
  through psql would therefore see all of them, not only its own.
- Setting the flag needs root in the overlay's netns on each host: a privileged global service (`pid: host`, NET_ADMIN, the
  netns directory) that follows Docker events and re-applies the flags. It is deployed by the stack (no manual install on a
  new server) but it is an agent in everything but name, and the start-up window above can only be narrowed by making the
  database refuse connections until the agent has set the flag.

Decision on the bridge flag: not adopted (a privileged agent per host and a start-up window, for isolation that the database can
enforce itself, see below).

## What was adopted instead (same day)

One internal overlay (`fridge_db`) for every database, and isolation by the database itself: the Postgres entrypoint starts
with `ssl=on`, `ssl_ca_file` holding only the node's own client certificate and a `pg_hba.conf` of
`hostssl all all all scram-sha-256 clientcert=verify-ca` / `host all all all reject`. The edge sidecar (`fridge forward --tls`)
sends PostgreSQL's SSLRequest, does the TLS handshake with that client certificate and verifies the server certificate; jobs
get the same material in their spec. No privileged agent, no start-up window (the rules live in the database's own config),
no per-database network.

Found while building it:
- libpq's default `channel_binding=prefer` made every client fail with "SCRAM channel binding check failed": the client
  binds to the certificate Traefik showed it, the database to its own. The fix is that they are the same certificate: the
  node's server certificate names both the public domain and `fridge-db-<key>`, and Traefik serves exactly it
  (`db_certs` was dropped for `db_tls`). `e2e.sh` runs a client with `channel_binding=require` to keep it so.
- `pg_isready` no longer proves anything about isolation (a rejected connection still counts as "accepting connections"),
  so the integration tests use psql and look for "requires a valid client certificate" / "pg_hba.conf rejects connection".
- Node certificates are valid five years (less if the CA expires first); Postgres reads them at start, so renewal means
  recreating the node. Reconcile alerts 60 days ahead.
