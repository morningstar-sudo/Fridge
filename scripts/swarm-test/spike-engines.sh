#!/usr/bin/env bash
# Spike: does every engine accept ONLY a client that presents the node's own client certificate, and is a client that
# presents another certificate (or none) refused at the TLS handshake? Runs plain `docker run` inside fsw1 (dind), one
# throwaway network per engine; certificates come from openssl in the alpine/openssl image.
# Usage: spike-engines.sh [redis|mongo|mysql|mariadb ...]   (default: all)
set -uo pipefail

D="docker exec fsw1 docker"
H="docker exec -i fsw1 sh -c"
WANT=${*:-redis mongo mysql mariadb}

$H 'rm -rf /root/spike && mkdir -p /root/spike' >/dev/null
$D run --rm -v /root/spike:/c --entrypoint sh alpine/openssl -c '
set -e
cd /c
openssl ecparam -name prime256v1 -genkey -noout -out ca.key
openssl req -x509 -new -key ca.key -sha256 -days 30 -subj "/CN=spike CA" -out ca.crt
openssl ecparam -name prime256v1 -genkey -noout -out server.key
openssl req -new -key server.key -subj "/CN=db" -out server.csr
printf "subjectAltName=DNS:db\nextendedKeyUsage=serverAuth\n" > ext.cnf
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 -sha256 -extfile ext.cnf -out server.crt
for n in client other; do
  openssl ecparam -name prime256v1 -genkey -noout -out $n.key
  openssl req -x509 -new -key $n.key -sha256 -days 30 -subj "/CN=$n" -addext "basicConstraints=critical,CA:TRUE" -addext "extendedKeyUsage=clientAuth" -out $n.crt
  cat $n.crt $n.key > $n.pem
done
cat server.crt server.key > server.pem
chmod 644 /c/*; chown -R 999:999 /c 2>/dev/null || true
' >/dev/null || { echo "certificate generation failed"; exit 1; }

net() { $D network rm "spike-$1" >/dev/null 2>&1; $D network create "spike-$1" >/dev/null; }
clean() { $D rm -f "spike-$1-db" >/dev/null 2>&1; $D network rm "spike-$1" >/dev/null 2>&1; }
# try <label> <expect ok|refused> <docker run args...>: runs a client container on the engine's network
try() {
	local what=$1 expect=$2; shift 2
	local out rc
	out=$("$@" 2>&1); rc=$?
	local got=refused; [ $rc -eq 0 ] && got=ok
	echo "$out" | grep -qE 'NOAUTH|WRONGPASS|AUTH failed|\(error\)|Could not connect|NOPERM' && got=refused # redis-cli exits 0 on a refused command
	printf '  %-46s %-8s (want %s)%s\n' "$what" "$got" "$expect" "$([ "$got" = "$expect" ] && echo "" || echo "   <<< MISMATCH: $(echo "$out" | tail -6 | tr '\n' ' ' | cut -c1-200)")"
}
waitfor() { # <seconds> <cmd...>
	local t=$1; shift
	for _ in $(seq 1 "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
	return 1
}

# ------------------------------------------------------------------ redis
spike_redis() {
	echo "== redis:7.4"; clean redis; net redis
	$H 'cat > /root/spike/redis.conf <<EOF
port 0
tls-port 6379
tls-cert-file /c/server.crt
tls-key-file /c/server.key
tls-ca-cert-file /c/client.crt
tls-auth-clients yes
aclfile /data/users.acl
EOF'
	$D run -d --name spike-redis-db --network spike-redis --network-alias db -v /root/spike:/c:ro redis:7.4 \
		sh -c '[ -f /data/users.acl ] || printf "user default off\nuser su on >superpw ~* &* +@all\n" > /data/users.acl; chown redis /data/users.acl; exec redis-server /c/redis.conf' >/dev/null
	cli() { $D run --rm --network spike-redis -v /root/spike:/c:ro redis:7.4 redis-cli --tls --cacert /c/ca.crt "$@"; }
	waitfor 30 cli --cert /c/client.crt --key /c/client.key -h db --user su --pass superpw ping || { echo "  redis did not come up"; $D logs spike-redis-db 2>&1 | tail -5; return; }
	try "own client cert, superuser" ok cli --cert /c/client.crt --key /c/client.key -h db --user su --pass superpw ping
	try "own client cert, ACL SETUSER + ACL SAVE" ok cli --cert /c/client.crt --key /c/client.key -h db --user su --pass superpw --no-auth-warning ACL SETUSER app on '>apppw' '~*' '+@all' '-@dangerous'
	try "own client cert, app user" ok cli --cert /c/client.crt --key /c/client.key -h db --user app --pass apppw set k v
	try "other client cert" refused cli --cert /c/other.crt --key /c/other.key -h db --user su --pass superpw ping
	try "no client cert" refused cli -h db --user su --pass superpw ping
	try "own cert, wrong password" refused cli --cert /c/client.crt --key /c/client.key -h db --user su --pass nope ping
	try "plain text on the TLS port" refused $D run --rm --network spike-redis redis:7.4 redis-cli -h db ping
	try "default user" refused cli --cert /c/client.crt --key /c/client.key -h db ping
	try "dump: redis-cli --rdb -" ok sh -c "$D run --rm --network spike-redis -v /root/spike:/c:ro redis:7.4 redis-cli --tls --cacert /c/ca.crt --cert /c/client.crt --key /c/client.key -h db --user su --pass superpw --rdb - | head -c 9 | grep -q REDIS"
	clean redis
}

# ------------------------------------------------------------------ mongo
spike_mongo() {
	echo "== mongo:7.0"; clean mongo; net mongo
	$D run -d --name spike-mongo-db --network spike-mongo --network-alias db -v /root/spike:/c:ro \
		-e MONGO_INITDB_ROOT_USERNAME=su -e MONGO_INITDB_ROOT_PASSWORD=superpw mongo:7.0 \
		mongod --tlsMode requireTLS --tlsCertificateKeyFile /c/server.pem --tlsCAFile /c/client.crt --auth --bind_ip_all >/dev/null
	sh_() { $D run --rm --network spike-mongo -v /root/spike:/c:ro mongo:7.0 mongosh --quiet "$@"; }
	waitfor 90 sh_ --host db --tls --tlsCertificateKeyFile /c/client.pem --tlsCAFile /c/ca.crt -u su -p superpw --authenticationDatabase admin --eval 'db.runCommand({ping:1}).ok' || { echo "  mongo did not come up"; $D logs spike-mongo-db 2>&1 | tail -8; return; }
	try "own client cert, superuser" ok sh_ --host db --tls --tlsCertificateKeyFile /c/client.pem --tlsCAFile /c/ca.crt -u su -p superpw --authenticationDatabase admin --eval 'db.runCommand({ping:1}).ok'
	try "own client cert, createUser on the node db" ok sh_ --host db --tls --tlsCertificateKeyFile /c/client.pem --tlsCAFile /c/ca.crt -u su -p superpw --authenticationDatabase admin --eval 'db.getSiblingDB("alice_x").createUser({user:"app",pwd:"apppw",roles:[{role:"readWrite",db:"alice_x"},{role:"dbAdmin",db:"alice_x"}]})'
	try "app user inserts" ok sh_ --host db --tls --tlsCertificateKeyFile /c/client.pem --tlsCAFile /c/ca.crt -u app -p apppw --authenticationDatabase alice_x alice_x --eval 'db.t.insertOne({a:1}).acknowledged'
	try "other client cert" refused sh_ --host db --tls --tlsCertificateKeyFile /c/other.pem --tlsCAFile /c/ca.crt -u su -p superpw --authenticationDatabase admin --eval 'db.runCommand({ping:1}).ok'
	try "no client cert" refused sh_ --host db --tls --tlsCAFile /c/ca.crt -u su -p superpw --authenticationDatabase admin --eval 'db.runCommand({ping:1}).ok'
	try "plain text" refused sh_ --host db -u su -p superpw --authenticationDatabase admin --eval 'db.runCommand({ping:1}).ok'
	try "own cert, wrong password" refused sh_ --host db --tls --tlsCertificateKeyFile /c/client.pem --tlsCAFile /c/ca.crt -u su -p nope --authenticationDatabase admin --eval 'db.runCommand({ping:1}).ok'
	try "mongodump --archive --gzip (stdout)" ok sh -c "$D run --rm --network spike-mongo -v /root/spike:/c:ro mongo:7.0 mongodump --uri \"mongodb://app:apppw@db/alice_x?authSource=alice_x&tls=true&tlsCertificateKeyFile=/c/client.pem&tlsCAFile=/c/ca.crt\" --archive --gzip | head -c 4 | od -An -tx1 | grep -q '1f 8b'"
	clean mongo
}

# ------------------------------------------------------------------ mysql / mariadb
spike_sql() { # <label> <image> <client binary> <extra server args...>
	local label=$1 image=$2 bin=$3; shift 3
	echo "== $image"; clean "$label"; net "$label"
	local envs=(-e MYSQL_ROOT_PASSWORD=rootpw -e MARIADB_ROOT_PASSWORD=rootpw)
	$D run -d --name "spike-$label-db" --network "spike-$label" --network-alias db -v /root/spike:/c:ro "${envs[@]}" "$image" \
		--ssl-cert=/c/server.crt --ssl-key=/c/server.key --ssl-ca=/c/client.crt "$@" >/dev/null
	cl() { $D run --rm --network "spike-$label" -v /root/spike:/c:ro -e MYSQL_PWD="${PW:-rootpw}" "$image" "$bin" -h db "$@"; }
	local own=(--ssl-cert=/c/client.crt --ssl-key=/c/client.key --ssl-ca=/c/ca.crt --ssl-verify-server-cert)
	[ "$bin" = mysql ] && own=(--ssl-mode=VERIFY_IDENTITY --ssl-cert=/c/client.crt --ssl-key=/c/client.key --ssl-ca=/c/ca.crt)
	waitfor 120 cl "${own[@]}" -uroot -e 'select 1' || { echo "  $label did not come up"; $D logs "spike-$label-db" 2>&1 | tail -8; return; }
	try "own client cert, root" ok cl "${own[@]}" -uroot -e 'select 1'
	try "create app user REQUIRE X509 (native password)" ok cl "${own[@]}" -uroot -e "CREATE DATABASE alice_x; CREATE USER 'alice_app'@'%' ${AUTH:-IDENTIFIED WITH mysql_native_password} BY 'apppw' REQUIRE X509; GRANT ALL ON alice_x.* TO 'alice_app'@'%'; ALTER USER 'root'@'%' REQUIRE X509;"
	PW=apppw try "app user, own cert" ok cl "${own[@]}" -ualice_app alice_x -e 'create table t(a int); insert into t values (1)'
	local other=("${own[@]}"); other=("${other[@]/\/c\/client.crt//c/other.crt}"); other=("${other[@]/\/c\/client.key//c/other.key}")
	PW=apppw try "app user, other client cert" refused cl "${other[@]}" -ualice_app alice_x -e 'select 1'
	PW=apppw try "app user, no client cert" refused cl --ssl-ca=/c/ca.crt -ualice_app alice_x -e 'select 1'
	PW=apppw try "app user, plain text" refused cl --ssl-mode=DISABLED -ualice_app alice_x -e 'select 1'
	PW=nope try "own cert, wrong password" refused cl "${own[@]}" -ualice_app alice_x -e 'select 1'
	PW=apppw try "dump to stdout" ok sh -c "$D run --rm --network spike-$label -v /root/spike:/c:ro -e MYSQL_PWD=apppw $image ${DUMP:-mysqldump} -h db ${own[*]} -ualice_app --single-transaction --routines --no-tablespaces alice_x | grep -q 'CREATE TABLE'"
	# --- the same database behind `fridge mysql-router`: clients present no certificate at all, only the router does
	j() { docker exec fsw1 cat "/root/spike/$1" | awk 'BEGIN{ORS="\\n"}1'; }
	printf '{"frontend":{"cert":"%s","key":"%s"},"users":{"alice_app":{"addr":"db:3306","cert":"%s","key":"%s","ca":"%s","server_name":"db"}}}' "$(j server.crt)" "$(j server.key)" "$(j client.crt)" "$(j client.key)" "$(j ca.crt)" | docker exec -i fsw1 sh -c 'cat > /root/spike/router.json'
	$D run -d --name "spike-$label-router" --network "spike-$label" --network-alias router -v /root/spike:/c:ro --entrypoint /fridge fridge:dev mysql-router --listen :3306 --config-file /c/router.json >/dev/null
	sleep 2
	rcl() { $D run --rm --network "spike-$label" -v /root/spike:/c:ro -e MYSQL_PWD="${PW:-apppw}" "$1" "$2" -h router -ualice_app "${@:3}"; }
	for pair in "mysql:8.4 mysql" "mariadb:11.4 mariadb"; do
		set -- $pair
		local mode=(--ssl-mode=VERIFY_CA --ssl-ca=/c/ca.crt); [ "$2" = mariadb ] && mode=(--ssl --skip-ssl-verify-server-cert)
		try "router: $2 client, TLS, no client cert" ok rcl "$1" "$2" "${mode[@]}" alice_x -e 'insert into t values (2); select count(*) from t'
		try "router: $2 client, 200 KB result" ok sh -c "$D run --rm --network spike-$label -v /root/spike:/c:ro -e MYSQL_PWD=apppw $1 $2 -h router -ualice_app ${mode[*]} alice_x -N -e \"select repeat('x',200000)\" | wc -c | tee /dev/stderr | grep -q 2000"
		PW=nope try "router: $2 client, wrong password" refused rcl "$1" "$2" "${mode[@]}" alice_x -e 'select 1'
	done
	try "router: plain text" refused rcl mysql:8.4 mysql --ssl-mode=DISABLED alice_x -e 'select 1'
	try "router: unknown user" refused $D run --rm --network "spike-$label" -v /root/spike:/c:ro -e MYSQL_PWD=x mysql:8.4 mysql -h router -unobody --ssl-mode=VERIFY_CA --ssl-ca=/c/ca.crt -e 'select 1'
	$D logs "spike-$label-router" 2>&1 | tail -3
	$D rm -f "spike-$label-router" >/dev/null 2>&1
	clean "$label"
}

for e in $WANT; do
	case "$e" in
	redis) spike_redis ;;
	mongo) spike_mongo ;;
	mysql) spike_sql mysql mysql:8.4 mysql --mysql-native-password=ON --require-secure-transport=ON ;;
	mariadb) AUTH="IDENTIFIED" DUMP=mariadb-dump spike_sql mariadb mariadb:11.4 mariadb --require-secure-transport=ON ;;
	*) echo "unknown engine $e" ;;
	esac
done
