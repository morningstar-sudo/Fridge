#!/usr/bin/env bash
# End to end on the two-node test swarm (up.sh with fridge:dev fridge-job:dev postgres:17 postgres:18 traefik:v3.5):
# installs the real stack with install.sh, then drives it with the CLI and the real clients (psql, redis-cli, mongosh, mysql,
# mariadb) the way users do:
#   - a database created through the CLI is reachable through the routing mesh on BOTH servers (SNI + TLS)
#   - an unknown name and a plain-text client are refused
#   - restarting Traefik loses nothing
#   - a database pinned to the other server survives that server being drained and brought back, data intact
#   - a vanished edge sidecar is recreated by the reconcile at boot
#   - every engine works through its public port on both servers with the CA the user downloads, and refuses plain text
#   - a server added with install.sh gets Traefik and databases
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
# SCRAM channel binding compares the certificate the client saw (Traefik's) with the database's own: they must be one
got=$(R PSQL "$DBPW" "$IP1" "channel_binding=require" -c "select count(*) from t" | tail -1)
[ "$got" = 1 ] || fail "channel_binding=require: got '$got'"
echo "channel binding ok"

step "each tenant has its own CA: verify-full works with the tenant's CA and not with the instance CA"
F ca --db e2eadm_e1 | docker exec -i fsw1 sh -c 'cat > /root/e1-ca.crt'
F ca | docker exec -i fsw1 sh -c 'cat > /root/instance-ca.crt'
VF() { # <ca file in fsw1>
	$D run --rm --network host -e PGPASSWORD="$DBPW" -v "$1:/ca.crt:ro" --entrypoint /usr/lib/postgresql/17/bin/psql fridge-job:dev \
		"host=db1.lan hostaddr=$IP1 port=5432 dbname=$DBNAME user=$DBNAME sslmode=verify-full sslrootcert=/ca.crt connect_timeout=8" -tAc "select 1"
}
[ "$(R VF /root/e1-ca.crt | tail -1)" = 1 ] || fail "verify-full with the tenant CA failed"
if VF /root/instance-ca.crt >/dev/null 2>&1; then fail "the instance CA vouched for a tenant's database"; fi
echo "tenant CA ok"

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


step "every engine through its public port, on both servers, with the CA the user downloads"
for _ in $(seq 1 40); do F ls >/dev/null 2>&1 && break; sleep 3; done # Fridge was just restarted by the reconcile step
# C <engine-name> <ip> <cmd...>: a client container (the job image carries every client) on fsw1's network, where <name>.lan is <ip>
C() {
	local host=$1 ip=$2; shift 2
	$D run --rm --network host --add-host "$host:$ip" -v /root/ca:/ca:ro --entrypoint sh fridge-job:dev -c "$*"
}
engine_check() { # <engine> <port> <client script using $HOST $USERNAME $PW $DB $CA, printing OK on success> <script that must fail: plain text>
	local eng=$1 port=$2 good=$3 plain=$4
	local name=e2e${eng} dom=${eng}1.lan
	F domains add "$dom" >/dev/null
	local conn user pw dbn
	conn=$(F create "$eng" "$name" --domain "$dom" --reveal | tail -1)
	user=$(echo "$conn" | sed -n 's|^[a-z]*://\([^:]*\):.*|\1|p')
	pw=$(echo "$conn" | sed -n 's|^[a-z]*://[^:]*:\([^@]*\)@.*|\1|p')
	dbn=e2eadm_$name
	[ -n "$user" ] && [ -n "$pw" ] || fail "$eng: no credentials in: $conn"
	docker exec fsw1 mkdir -p /root/ca
	F ca --db "e2eadm_$name" | docker exec -i fsw1 sh -c "cat > /root/ca/$eng.crt"
	for ip in "$IP1" "$IP2"; do
		got=""
		for _ in $(seq 1 25); do # Traefik and the router poll Fridge every 5s
			got=$(C "$dom" "$ip" "HOST=$dom USERNAME='$user' PW='$pw' DB=$dbn CA=/ca/$eng.crt; $good" 2>&1 | tail -3 || true)
			case "$got" in *OK*) break ;; esac
			sleep 3
		done
		case "$got" in *OK*) ;; *) fail "$eng through $ip: $got" ;; esac
		if C "$dom" "$ip" "HOST=$dom USERNAME='$user' PW='$pw' DB=$dbn; $plain" >/dev/null 2>&1; then fail "$eng: plain text was served through $ip"; fi
	done
	echo "$eng ok on $IP1 and $IP2"
}
engine_check redis 6379 \
	'redis-cli --tls --cacert $CA -h $HOST --sni $HOST --user $USERNAME --pass $PW --no-auth-warning set k v | grep -q OK && redis-cli --tls --cacert $CA -h $HOST --sni $HOST --user $USERNAME --pass $PW --no-auth-warning get k | grep -q "^v$" && echo OK' \
	'redis-cli -h $HOST --user $USERNAME --pass $PW --no-auth-warning ping | grep -q PONG'
engine_check mongo 27017 \
	'mongosh --quiet "mongodb://$USERNAME:$PW@$HOST:27017/$DB?tls=true&tlsCAFile=$CA" --eval "db.t.insertOne({a:1}).acknowledged" | grep -q true && echo OK' \
	'mongosh --quiet "mongodb://$USERNAME:$PW@$HOST:27017/$DB?tls=false&serverSelectionTimeoutMS=4000" --eval "db.runCommand({ping:1}).ok" | grep -qx 1'
engine_check mysql 3306 \
	'export MYSQL_PWD=$PW; mysql -h $HOST -u $USERNAME --ssl-mode=VERIFY_CA --ssl-ca=$CA $DB -e "create table if not exists t(a int); insert into t values (1); select count(*) from t" | grep -q "^[1-9]" && echo OK' \
	'export MYSQL_PWD=$PW; mysql -h $HOST -u $USERNAME --ssl-mode=DISABLED $DB -e "select 1" | grep -q 1'
engine_check mariadb 3306 \
	'export MYSQL_PWD=$PW; mariadb -h $HOST -u $USERNAME --ssl --ssl-ca=$CA --skip-ssl-verify-server-cert $DB -e "create table if not exists t(a int); insert into t values (1); select count(*) from t" | grep -q "^[1-9]" && echo OK' \
	'export MYSQL_PWD=$PW; mariadb -h $HOST -u $USERNAME --skip-ssl $DB -e "select 1" | grep -q 1'

step "adding a server: --add-node prints the command, then does it over SSH; the new server gets Traefik and databases"
bash scripts/swarm-test/node.sh fsw3 fridge:dev fridge-job:dev postgres:17 postgres:18 traefik:v3.5 >/dev/null
export MGR_DOCKER=tcp://127.0.0.1:23750 # the installer runs on this host and talks to the manager's API, as it would on the manager
out=$(DOCKER_HOST=$MGR_DOCKER sh install.sh --add-node)
echo "$out" | grep -q -- "--join .*:2377 --token SWMTKN-" || fail "--add-node printed no join command: $out"
echo "$out" | grep -q "docker swarm join-token --rotate worker" || fail "--add-node does not say how to revoke the token"
# the test containers have no sshd: an ssh that runs the command inside fsw3 stands in for it
SHIM=$(mktemp -d)
printf '#!/bin/sh\nshift\nexec env -u DOCKER_HOST docker exec -i fsw3 "$@"\n' >"$SHIM/ssh"
chmod +x "$SHIM/ssh"
DOCKER_HOST=$MGR_DOCKER PATH="$SHIM:$PATH" sh install.sh --add-node worker@fsw3 | tail -6
rm -rf "$SHIM"
for _ in $(seq 1 60); do
	[ "$($D service ls --filter name=fridge_traefik --format '{{.Replicas}}')" = "3/3" ] && break
	sleep 3
done
[ "$($D service ls --filter name=fridge_traefik --format '{{.Replicas}}')" = "3/3" ] || fail "Traefik did not start on the new server"
echo "Traefik runs on all 3 servers"
F domains add db3.lan >/dev/null
F create postgres e3 --domain db3.lan --version 17 --reveal >/dev/null
S3=""; for s in $($D service ls --filter label=fridge.role=db --format '{{.Name}}'); do case "$($D service ps "$s" --filter desired-state=running --format '{{.Node}}')" in fsw3) S3=$s ;; esac; done
[ -n "$S3" ] || fail "no database was placed on the new server (it was not labelled: it must be eligible by default)"
echo "database on the new server: $S3"
IP3=$(docker inspect -f '{{(index .NetworkSettings.Networks "fsw-net").IPAddress}}' fsw3)
DBPW3=$(F show e2eadm_e3 --reveal | grep -o 'postgresql://[^ ]*' | head -1 | sed -n 's|^postgresql://[^:]*:\([^@]*\)@.*|\1|p')
PSQL3() { $D run --rm --network host -e PGPASSWORD="$DBPW3" --entrypoint /usr/lib/postgresql/17/bin/psql fridge-job:dev \
	"host=db3.lan hostaddr=$1 port=5432 dbname=e2eadm_e3 user=e2eadm_e3 sslmode=require connect_timeout=8" -tA "${@:2}"; }
for ip in "$IP3" "$IP1"; do # through the new server's own Traefik, and through an old one reaching a database on the new server
	[ "$(R PSQL3 "$ip" -c 'select 7' | tail -1)" = 7 ] || fail "e3 through $ip"
done
echo "new server works"

step "all good"
