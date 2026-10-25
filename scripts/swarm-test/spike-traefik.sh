#!/usr/bin/env bash
# P0 spike, part 2: Traefik with the HTTP provider (cert inline) -> edge sidecar -> database, entered through
# the routing mesh of the *other* node. Needs the swarm from up.sh and the networks/services of spike.sh part 1
# (this script recreates what it needs).
set -uo pipefail
D() { docker exec fsw1 docker "$@"; }
ok() { echo "PASS $*"; }
bad() { echo "FAIL $*"; FAILED=1; }
W=$(mktemp -d)

D service rm fridge-db-a fridge-edge-a tprov traefik j-sidecars >/dev/null 2>&1
D network rm fridge-node-a fridge_edge fridge_egress >/dev/null 2>&1; sleep 3
D network create -d overlay --attachable fridge_edge >/dev/null
D network create -d overlay --internal --opt encrypted fridge-node-a >/dev/null
D network create -d overlay fridge_egress >/dev/null
D service create -d --name fridge-db-a --constraint node.hostname==fsw2 --network fridge-node-a --endpoint-mode dnsrr \
	--mount type=volume,source=fridge-postgres-a,target=/var/lib/postgresql/data -e POSTGRES_PASSWORD=pw postgres:17 >/dev/null
D service create -d --name fridge-edge-a --constraint node.hostname==fsw2 --network fridge_edge --network fridge-node-a \
	--entrypoint /fridge fridge:dev forward --listen :5432 --to fridge-db-a:5432 >/dev/null 2>&1

openssl req -x509 -newkey rsa:2048 -nodes -keyout "$W/k.pem" -out "$W/c.pem" -days 2 -subj "/CN=db-a.lan" -addext "subjectAltName=DNS:db-a.lan" 2>/dev/null
cat >"$W/gen.js" <<'JS'
const fs = require("fs");
const w = process.argv[2];
const c = fs.readFileSync(w + "/c.pem", "utf8"), k = fs.readFileSync(w + "/k.pem", "utf8");
fs.writeFileSync(w + "/t.json", JSON.stringify({
  tcp: { routers: { "db-a": { entryPoints: ["pg"], rule: "HostSNI(`db-a.lan`)", service: "db-a", tls: { options: "pg" } } },
         services: { "db-a": { loadBalancer: { servers: [{ address: "fridge-edge-a:5432" }] } } } },
  tls: { certificates: [{ certFile: c, keyFile: k }], options: { pg: { alpnProtocols: ["postgresql"] } } },
}));
JS
node "$W/gen.js" "$W"
D config rm tjson >/dev/null 2>&1
docker exec -i fsw1 docker config create tjson - <"$W/t.json" >/dev/null && ok "dynamic config with inline cert stored as a Docker config"
D service create -d --name tprov --network fridge_edge --config source=tjson,target=/www/traefik.json busybox:1.37 httpd -f -p 8081 -h /www >/dev/null
D service create -d --name traefik --network fridge_edge --constraint node.hostname==fsw1 -p 5432:5432 traefik:v3.5 \
	--entrypoints.pg.address=:5432 --providers.http.endpoint=http://tprov:8081/traefik.json --providers.http.pollInterval=2s --log.level=INFO >/dev/null
sleep 30
ip2=$(docker inspect -f '{{(index .NetworkSettings.Networks "fsw-net").IPAddress}}' fsw2)
echo "entering through fsw2 ($ip2); Traefik runs on fsw1; the database and its sidecar on fsw2"
out=$(docker run --rm --network fsw-net -e PGPASSWORD=pw postgres:17 psql "host=db-a.lan hostaddr=$ip2 port=5432 user=postgres sslmode=require" -tAc "select ssl from pg_stat_ssl where pid=pg_backend_pid()" 2>&1)
echo "result: $out"
[ "$out" = "f" ] && ok "psql sslmode=require via mesh -> Traefik (TLS terminated, cert from HTTP provider) -> sidecar -> database" || bad "mesh path: $out"
out=$(docker run --rm --network fsw-net -v "$W:/w" -e PGPASSWORD=pw postgres:17 psql "host=db-a.lan hostaddr=$ip2 port=5432 user=postgres sslmode=verify-full sslrootcert=/w/c.pem" -tAc "select 1" 2>&1)
[ "$out" = "1" ] && ok "verify-full against the cert served from the provider config" || bad "verify-full: $out"
out=$(docker run --rm --network fsw-net -e PGPASSWORD=pw postgres:17 psql "host=other.lan hostaddr=$ip2 port=5432 user=postgres sslmode=require" -tAc "select 1" 2>&1 | head -1)
[ "$out" = "1" ] && bad "unknown SNI got through" || ok "unknown SNI is refused ($out)"
D service logs traefik 2>&1 | grep -ci "error" | sed 's/^/traefik error lines: /'

echo "== sidecars are not visible from a job that only has the egress network =="
D service create -d --name j-sidecars --mode replicated-job --restart-condition none --network fridge_egress busybox:1.37 sh -c 'nc -z -w 3 fridge-edge-a 5432; echo "rc=$?"' >/dev/null
sleep 8; D service logs j-sidecars 2>&1 | tail -1
D service logs j-sidecars 2>&1 | grep -q 'rc=0' && bad "egress-only job reaches the sidecar" || ok "egress-only job cannot reach the sidecar"

echo "== job with no network at all (instead of --network none) =="
D service rm j-nonet >/dev/null 2>&1
D service create -d --name j-nonet --mode replicated-job --restart-condition none busybox:1.37 sh -c 'ip -o addr | grep -v " lo " | wc -l; wget -q -T 3 -O- http://example.com >/dev/null; echo "rc=$?"' >/dev/null
sleep 8; D service logs j-nonet 2>&1 | tail -3
D service logs j-nonet 2>&1 | grep -q 'rc=0' && echo "INFO a task with no network still has egress (so the isolated sandbox is an empty internal overlay, not 'no network')" || ok "no-network job has no internet"

D service rm fridge-db-a fridge-edge-a tprov traefik j-sidecars j-nonet >/dev/null 2>&1
rm -rf "$W"
exit ${FAILED:-0}
