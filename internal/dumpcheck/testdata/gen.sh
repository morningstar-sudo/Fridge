#!/usr/bin/env bash
# Regenerates the real dump files used by the tests (needs docker; run from anywhere).
#   pg14.dump / pg17.dump        server N, pg_dump N
#   pg13-by-pgdump17.dump        server 13, dumped by pg_dump 17 (a newer pg_dump than the server)
#   plain.sql.gz                 plain SQL from pg_dump 17, gzipped
set -euo pipefail
cd "$(dirname "$0")"
SQL="CREATE TABLE note(id serial PRIMARY KEY, body text); INSERT INTO note(body) SELECT 'row ' || g FROM generate_series(1,20) g;"
run() { # <version> <name>: starts a throwaway server of that version
	docker rm -f "gen-$2" >/dev/null 2>&1 || true
	docker run -d --name "gen-$2" -e POSTGRES_HOST_AUTH_METHOD=trust "postgres:$1" >/dev/null
	until docker exec "gen-$2" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; do sleep 1; done
	sleep 2
	docker exec "gen-$2" psql -h 127.0.0.1 -U postgres -q -c "$SQL"
}
for v in 14 17; do
	run $v $v
	docker exec "gen-$v" pg_dump -h 127.0.0.1 -U postgres -Fc postgres > "pg$v.dump"
done
docker exec gen-17 pg_dump -h 127.0.0.1 -U postgres postgres | gzip -9 > plain.sql.gz
run 13 13
docker run --rm --network "container:gen-13" postgres:17 pg_dump -h 127.0.0.1 -U postgres -Fc postgres > pg13-by-pgdump17.dump
docker rm -f gen-14 gen-17 gen-13 >/dev/null
# a 1x1 PNG and an empty file
printf '\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xcf\xc0\xf0\x1f\x00\x05\x00\x01\xff\xa5\xeb\x1b\xa8\x00\x00\x00\x00IEND\xaeB`\x82' > not-a-dump.dump
: > empty.dump
ls -l
