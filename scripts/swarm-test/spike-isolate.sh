#!/usr/bin/env bash
# Spike: can `bridge link set dev <veth> isolated on` replace one overlay per database?
# One shared overlay "shared-db"; dbA, dbB (fsw1) and dbC (fsw2) are fake databases, sc1 (fsw1) is a sidecar.
# Isolated ports reach only non-isolated ports; the flags go on the DB veths and on vxlan0, inside the overlay's netns.
# Needs the dind swarm from up.sh (with busybox:1.37). Prints a matrix of who reaches whom before and after.
set -uo pipefail

M="docker exec fsw1 docker"
for n in fsw1 fsw2; do docker exec $n apk add --no-cache iproute2 >/dev/null 2>&1; done

$M service rm dbA dbB dbC sc1 >/dev/null 2>&1
for _ in $(seq 1 30); do $M network rm shared-db >/dev/null 2>&1; $M network inspect shared-db >/dev/null 2>&1 || break; sleep 2; done
sleep 3
$M network create -d overlay ${ENCRYPTED:+--opt encrypted} shared-db >/dev/null
NETID=$($M network inspect shared-db --format '{{.Id}}')
NETNS=/var/run/docker/netns/1-${NETID:0:10}

svc() { # <name> <node>
	$M service create -d --name "$1" --network shared-db --endpoint-mode dnsrr --constraint "node.hostname==$2" \
		busybox:1.37 sh -c 'while true; do echo hi | nc -l -p 5432; done' >/dev/null
}
svc dbA fsw1; svc dbB fsw1; svc dbC fsw2; svc sc1 fsw1

ctr() { docker exec "$1" docker ps -q --filter "name=$2." | head -1; } # <node> <service>
for _ in $(seq 1 40); do
	[ -n "$(ctr fsw1 dbA)" ] && [ -n "$(ctr fsw1 dbB)" ] && [ -n "$(ctr fsw2 dbC)" ] && [ -n "$(ctr fsw1 sc1)" ] && break
	sleep 2
done
sleep 5

probe() { # <from node> <from service> <to service>: "OPEN" when a TCP connection and a reply work
	local c; c=$(ctr "$1" "$2")
	if docker exec "$1" docker exec "$c" sh -c "nc -w 2 $3 5432 </dev/null" 2>/dev/null | grep -q hi; then echo OPEN; else echo blocked; fi
}
matrix() {
	printf '  %-22s %s\n' "dbA -> dbB (same host)" "$(probe fsw1 dbA dbB)"
	printf '  %-22s %s\n' "dbA -> dbC (other host)" "$(probe fsw1 dbA dbC)"
	printf '  %-22s %s\n' "dbC -> dbA (other host)" "$(probe fsw2 dbC dbA)"
	printf '  %-22s %s\n' "dbC -> dbB (other host)" "$(probe fsw2 dbC dbB)"
	printf '  %-22s %s\n' "sc1 -> dbA (same host)" "$(probe fsw1 sc1 dbA)"
	printf '  %-22s %s\n' "sc1 -> dbB (same host)" "$(probe fsw1 sc1 dbB)"
	printf '  %-22s %s\n' "sc1 -> dbC (other host)" "$(probe fsw1 sc1 dbC)"
}

# veth of a container inside the overlay netns of its node
veth_of() { # <node> <service>
	local c idx
	c=$(ctr "$1" "$2")
	for idx in $(docker exec "$1" docker exec "$c" sh -c 'for f in /sys/class/net/eth*/ifindex; do cat $f; done'); do
		v=$(docker exec "$1" nsenter --net="$NETNS" ip -o link | sed -n "s/^[0-9]*: \(veth[^@]*\)@if$idx:.*/\1/p")
		[ -n "$v" ] && { echo "$v"; return; }
	done
}
isolate() { # <node> <dev>
	docker exec "$1" nsenter --net="$NETNS" bridge link set dev "$2" isolated on
}
show() { docker exec "$1" nsenter --net="$NETNS" bridge -d link show | grep -o '^[0-9]*: [^ ]*\|isolated [a-z]*' | paste -sd' ' | sed 's/ \([0-9]*:\)/\n  \1/g' | sed 's/^/  /'; }

echo "== before: nothing isolated"
matrix

echo "== isolating dbA, dbB (fsw1), dbC (fsw2) and vxlan0 on both hosts"
A=$(veth_of fsw1 dbA); B=$(veth_of fsw1 dbB); C=$(veth_of fsw2 dbC)
echo "  veths: dbA=$A dbB=$B dbC=$C"
isolate fsw1 "$A"; isolate fsw1 "$B"; isolate fsw2 "$C"
isolate fsw1 vxlan0 2>&1 | sed 's/^/  fsw1 vxlan0: /'; isolate fsw2 vxlan0 2>&1 | sed 's/^/  fsw2 vxlan0: /'
echo "  fsw1 ports:"; show fsw1
echo "== after"
matrix

echo "== after only the DB veths (vxlan0 left open): cross-host leak check"
docker exec fsw1 nsenter --net="$NETNS" bridge link set dev vxlan0 isolated off
docker exec fsw2 nsenter --net="$NETNS" bridge link set dev vxlan0 isolated off
matrix
docker exec fsw1 nsenter --net="$NETNS" bridge link set dev vxlan0 isolated on
docker exec fsw2 nsenter --net="$NETNS" bridge link set dev vxlan0 isolated on

echo "== restart dbA: is the new task isolated?"
OLD=$(ctr fsw1 dbA)
$M service update -d --force dbA >/dev/null
NEW=""
for _ in $(seq 1 60); do
	c=$(ctr fsw1 dbA)
	if [ -n "$c" ] && [ "$c" != "$OLD" ] && [ "$(docker exec fsw1 docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null)" = true ]; then NEW=$c; break; fi
	sleep 1
done
sleep 3
A2=$(veth_of fsw1 dbA)
echo "  new container ${NEW:0:12}, veth: $A2 (was $A)"
echo "  flags: $(docker exec fsw1 nsenter --net="$NETNS" bridge -d link show dev "$A2" | grep -o 'isolated [a-z]*')"
echo "  dbA(new) -> dbB: $(probe fsw1 dbA dbB)   <- OPEN: the new port starts open, until something sets the flag"
echo "  dbB -> dbA(new): $(probe fsw1 dbB dbA)"
echo "  sc1 -> dbA(new): $(probe fsw1 sc1 dbA)"
$M service rm dbA dbB dbC sc1 >/dev/null 2>&1
