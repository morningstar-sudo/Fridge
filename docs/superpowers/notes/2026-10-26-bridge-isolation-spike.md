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

Decision: not adopted. One overlay per database stays the isolation mechanism (plain Docker configuration, no privileged
component, no window). Revisit when the number of databases per swarm approaches several hundred; the cheaper levers first
are a smaller `--default-addr-pool-mask-length` at `swarm init`, then Unix sockets in a volume shared with the sidecar.
