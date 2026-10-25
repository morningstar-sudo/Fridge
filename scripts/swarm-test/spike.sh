#!/usr/bin/env bash
# P0 spike: the Swarm behaviours the design leans on, checked in the two-node dind swarm from up.sh
# (needs images fridge:dev, postgres:17, traefik:v3.5, busybox:1.37 in both nodes). Prints PASS/FAIL per claim.
set -uo pipefail
D() { docker exec fsw1 docker "$@"; }
ok() { echo "PASS $*"; }
bad() { echo "FAIL $*"; FAILED=1; }
wait_job() { # <name> [seconds] -> prints the job's final task state
	for _ in $(seq 1 "${2:-90}"); do
		st=$(D service ps "$1" --format '{{.CurrentState}}' --no-trunc 2>/dev/null | head -1)
		case $st in Complete*|Failed*|Rejected*) echo "$st"; return;; esac
		sleep 1
	done
	echo "timeout ($st)"
}
job() { # <name> <networks,comma> <image> <cmd...> : one-shot job, output = its logs
	local name=$1 nets=$2 img=$3; shift 3
	local args=()
	for n in ${nets//,/ }; do args+=(--network "$n"); done
	D service rm "$name" >/dev/null 2>&1
	D service create -d --name "$name" --mode replicated-job --restart-condition none "${args[@]}" "$img" "$@" >/dev/null
	local st; st=$(wait_job "$name")
	echo "[$name: $st]"
	D service logs "$name" 2>&1 | sed 's/^[^ ]* *| //' | tail -${LOGLINES:-5}
}

D service rm fridge-db-a fridge-edge-a fridge-db-b tprov traefik >/dev/null 2>&1
D network rm fridge-node-a fridge-node-b fridge_edge fridge_egress >/dev/null 2>&1
sleep 3

echo "== networks =="
D network create -d overlay --attachable fridge_edge >/dev/null && ok "fridge_edge overlay"
D network create -d overlay fridge_egress >/dev/null && ok "fridge_egress overlay (non-internal)"
D network create -d overlay --internal --opt encrypted fridge-node-a >/dev/null && ok "fridge-node-a overlay internal+encrypted"
D network create -d overlay --internal --opt encrypted fridge-node-b >/dev/null && ok "fridge-node-b overlay internal+encrypted"

echo "== database service pinned to fsw2, sidecar on the same node =="
D service create -d --name fridge-db-a --constraint node.hostname==fsw2 --network fridge-node-a --endpoint-mode dnsrr \
	--mount type=volume,source=fridge-postgres-a,target=/var/lib/postgresql/data -e POSTGRES_PASSWORD=pw postgres:17 >/dev/null && ok "db service created"
D service create -d --name fridge-db-b --constraint node.hostname==fsw1 --network fridge-node-b --endpoint-mode dnsrr \
	--mount type=volume,source=fridge-postgres-b,target=/var/lib/postgresql/data -e POSTGRES_PASSWORD=pw postgres:17 >/dev/null && ok "second db service (other node) created"
D service create -d --name fridge-edge-a --constraint node.hostname==fsw2 --network fridge_edge --network fridge-node-a \
	--entrypoint /fridge fridge:dev forward --listen :5432 --to fridge-db-a:5432 >/dev/null && ok "edge sidecar created with --entrypoint override"
sleep 25
node=$(D service ps fridge-db-a --format '{{.Node}} {{.CurrentState}}' | head -1); echo "db task: $node"
case $node in fsw2\ Running*) ok "db task runs on the pinned node";; *) bad "db task: $node";; esac
vol=$(docker exec fsw2 docker volume ls -q | grep -c fridge-postgres-a); [ "$vol" = 1 ] && ok "volume created on fsw2 only" || bad "volume on fsw2: $vol"
[ "$(docker exec fsw1 docker volume ls -q | grep -c fridge-postgres-a)" = 0 ] && ok "volume absent on fsw1" || bad "volume leaked to fsw1"

echo "== job on the edge network reaches the database through the sidecar =="
out=$(job j-edge fridge_edge,fridge_egress postgres:17 sh -c 'until pg_isready -h fridge-edge-a -p 5432 -t 2; do sleep 1; done; PGPASSWORD=pw psql -h fridge-edge-a -U postgres -tAc "select 1+1"')
echo "$out"; echo "$out" | grep -q 'Complete' && echo "$out" | grep -qx '2' && ok "job completed, exit code ok, output readable from service logs" || bad "edge job"
echo "$(D service inspect j-edge --format '{{.JobStatus}}' 2>/dev/null)"

echo "== a failing job reports failure =="
out=$(LOGLINES=2 job j-fail fridge_edge busybox:1.37 sh -c 'echo oops >&2; exit 3'); echo "$out"
echo "$out" | grep -q 'Failed' && ok "failed job shows Failed (exit code in task status)" || bad "failing job state"
D service ps j-fail --format '{{.Error}}' --no-trunc | head -1

echo "== isolation =="
out=$(job j-iso fridge-node-b postgres:17 sh -c 'pg_isready -h fridge-db-a -p 5432 -t 3; echo "rc=$?"'); echo "$out"
echo "$out" | grep -q 'rc=[12]' && ok "node-b network cannot reach db a" || bad "isolation: $out"
out=$(job j-net fridge-node-a busybox:1.37 sh -c 'wget -q -T 4 -O- http://example.com >/dev/null; echo "rc=$?"'); echo "$out"
echo "$out" | grep -q 'rc=0' && bad "internal network has internet" || ok "internal overlay has no egress"
out=$(job j-egress fridge-node-a,fridge_egress busybox:1.37 sh -c 'wget -q -T 6 -O- http://example.com >/dev/null; echo "rc=$?"'); echo "$out"
echo "$out" | grep -q 'rc=0' && ok "node net + fridge_egress gives egress" || bad "no egress with fridge_egress: $out"
out=$(job j-egress2 fridge_egress busybox:1.37 sh -c 'nc -z -w 3 fridge-edge-a 5432; echo "rc=$?"'); echo "$out"
echo "$out" | grep -q 'rc=0' && bad "job with fridge_egress can see the edge sidecars" || ok "fridge_egress does not expose edge sidecars"

echo "== service --network none =="
D service create -d --name j-none --mode replicated-job --network none busybox:1.37 true >/dev/null 2>&1 && echo "INFO --network none accepted" || echo "INFO --network none is refused for services (use an empty internal overlay as the sandbox)"
echo "(Traefik / routing mesh part: spike-traefik.sh)"
echo "== cleanup =="
D service rm fridge-db-a fridge-edge-a fridge-db-b j-edge j-fail j-iso j-net j-egress j-egress2 j-none >/dev/null 2>&1
exit ${FAILED:-0}
