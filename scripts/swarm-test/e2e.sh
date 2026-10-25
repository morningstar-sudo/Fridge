#!/usr/bin/env bash
# End to end on the two-node test swarm (up.sh with fridge:dev fridge-job:dev postgres:17 postgres:18 traefik:v3.5):
# installs the real stack with install.sh, then drives it with the CLI and psql the way users do:
#   - a database created through the CLI is reachable through the routing mesh on BOTH servers (SNI + TLS)
#   - an unknown name and a plain-text client are refused
#   - restarting Traefik loses nothing
#   - a database pinned to the other server survives that server being drained and brought back, data intact
#   - a vanished edge sidecar is recreated by the reconcile at boot
set -euo pipefail
cd "$(dirname "$0")/../.."

D="docker exec -i fsw1 docker"
step() { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

step "install"
OUT=$(bash scripts/swarm-test/install.sh --admin e2eadm)
echo "$OUT" | tail -12
PW=$(echo "$OUT" | sed -n 's/.*password: \([0-9a-f]*\).*/\1/p')
[ -n "$PW" ] || fail "the installer printed no admin password"

F() { $D run --rm -i --network host -v /root/cfg:/cfg -e FRIDGE_CONFIG_DIR=/cfg -e FRIDGE_URL=https://localhost:8443 --entrypoint /fridge fridge:dev "$@"; }
PSQL() { # PSQL <pw> <hostaddr> <conninfo extras> -- <psql args>
	local pw=$1 ip=$2 extra=$3; shift 3
	$D run --rm --network host -e PGPASSWORD="$pw" --entrypoint /usr/lib/postgresql/17/bin/psql fridge-job:dev \
		"host=db1.lan hostaddr=$ip port=5432 dbname=$DBNAME user=$DBNAME sslmode=require connect_timeout=8 $extra" -tA "$@"
}
# R <cmd...>: run it until it works; each new connection may land on a Traefik replica that has not polled yet
R() { local out i; for i in $(seq 1 20); do out=$("$@" 2>/dev/null) && { printf '%s
' "$out"; return 0; }; sleep 2; done; return 1; }
IP1=$(docker inspect -f '{{(index .NetworkSettings.Networks "fsw-net").IPAddress}}' fsw1)
IP2=$(docker inspect -f '{{(index .NetworkSettings.Networks "fsw-net").IPAddress}}' fsw2)

step "login and create a database"
echo "$PW" | F login --user e2eadm --password-stdin --yes
F domains add db1.lan >/dev/null
F domains add db2.lan >/dev/null
CONN=$(F create postgres e1 --domain db1.lan --version 17 --reveal | tail -1)
DBNAME=e2eadm_e1
DBPW=$(echo "$CONN" | sed -n 's|^postgresql://[^:]*:\([^@]*\)@.*|\1|p')
[ -n "$DBPW" ] || fail "no password in: $CONN"

step "reachable through the mesh on both servers"
for ip in "$IP1" "$IP2"; do
	got=""
	for _ in $(seq 1 20); do # Traefik polls Fridge every 5s: the new route may take a moment
		got=$(PSQL "$DBPW" "$ip" "" -c "create table if not exists t(x int); select count(*) from t" 2>/dev/null | tail -1 || true)
		[ -n "$got" ] && break
		sleep 3
	done
	[ "$got" = 0 ] || fail "via $ip: got '$got'"
done
R PSQL "$DBPW" "$IP1" "" -c "insert into t values (1)" >/dev/null
echo "ok on $IP1 and $IP2"

step "unknown name and plain text are refused"
if $D run --rm --network host -e PGPASSWORD="$DBPW" --entrypoint /usr/lib/postgresql/17/bin/psql fridge-job:dev \
	"host=nope.lan hostaddr=$IP1 port=5432 dbname=$DBNAME user=$DBNAME sslmode=require connect_timeout=5" -tAc "select 1" >/dev/null 2>&1; then fail "an unknown name was served"; fi
if $D run --rm --network host -e PGPASSWORD="$DBPW" --entrypoint /usr/lib/postgresql/17/bin/psql fridge-job:dev \
	"host=db1.lan hostaddr=$IP1 port=5432 dbname=$DBNAME user=$DBNAME sslmode=disable connect_timeout=5" -tAc "select 1" >/dev/null 2>&1; then fail "plain text was served"; fi
echo refused

step "restarting Traefik loses nothing"
$D service update -d --force fridge_traefik >/dev/null
sleep 5
for _ in $(seq 1 30); do
	got=$(PSQL "$DBPW" "$IP2" "" -c "select count(*) from t" 2>/dev/null | tail -1 || true)
	[ "$got" = 1 ] && break
	sleep 3
done
[ "${got:-}" = 1 ] || fail "after the restart: got '${got:-}'"
echo ok

step "a database on the other server: placed there, drained, back with its data"
# the first database filled fsw1's quota: the next one lands on the emptier server
F create postgres e2 --domain db2.lan --version 17 --reveal >/dev/null
KEY=$($D service ls --filter label=fridge.role=db --format '{{.Name}}' | tr '\n' ' ')
HOSTS=""
for s in $KEY; do HOSTS="$HOSTS $s@$($D service ps "$s" --filter desired-state=running --format '{{.Node}}')"; done
echo "databases:$HOSTS"
case "$HOSTS" in *@fsw1*@fsw2*|*@fsw2*@fsw1*) ;; *) fail "both databases are on one server:$HOSTS" ;; esac
S2=$(echo "$HOSTS" | tr ' ' '\n' | sed -n 's/^\(.*\)@fsw2$/\1/p' | head -1)
DBNAME2=e2eadm_e2
CONN2=$(F show e2eadm_e2 --reveal | grep -o 'postgresql://[^ ]*' | head -1)
DBPW2=$(echo "$CONN2" | sed -n 's|^postgresql://[^:]*:\([^@]*\)@.*|\1|p')
[ -n "$DBPW2" ] || fail "no password for e2: $CONN2"
PSQL2() { $D run --rm --network host -e PGPASSWORD="$DBPW2" --entrypoint /usr/lib/postgresql/17/bin/psql fridge-job:dev \
	"host=db2.lan hostaddr=$1 port=5432 dbname=$DBNAME2 user=$DBNAME2 sslmode=require connect_timeout=8" -tA "${@:2}"; }
for _ in $(seq 1 20); do # the route appears within one Traefik poll
	PSQL2 "$IP1" -c "create table if not exists t2(x int)" >/dev/null 2>&1 && break
	sleep 3
done
R PSQL2 "$IP1" -c "insert into t2 values (7)" >/dev/null
[ "$(R PSQL2 "$IP1" -c "select x from t2" | tail -1)" = 7 ] || fail "e2 through fsw1 (its database is on fsw2)"
$D node update --availability drain fsw2 >/dev/null
sleep 15
if PSQL2 "$IP1" -c 'select 1' >/dev/null 2>&1; then fail "e2 answered while its server was drained"; fi
echo "down while drained"
[ "$(R PSQL "$DBPW" "$IP1" "" -c "select count(*) from t" | tail -1)" = 1 ] || fail "e1 was disturbed by draining fsw2"
$D node update --availability active fsw2 >/dev/null
got=""
for _ in $(seq 1 60); do
	got=$(PSQL2 "$IP1" -c 'select x from t2' 2>/dev/null | tail -1 || true)
	[ "$got" = 7 ] && break
	sleep 3
done
[ "$got" = 7 ] || fail "e2 did not come back with its data (got '$got')"
echo "back, data intact"

step "a vanished sidecar is recreated at boot"
EDGE=$($D service ls --filter label=fridge.role=edge --format '{{.Name}}' | head -1)
$D service rm "$EDGE" >/dev/null
$D service update -d --force fridge_fridge >/dev/null
for _ in $(seq 1 60); do
	$D service ls --format '{{.Name}}' | grep -qx "$EDGE" && break
	sleep 3
done
$D service ls --format '{{.Name}}' | grep -qx "$EDGE" || fail "$EDGE was not recreated"
echo "recreated $EDGE"

step "all good"
