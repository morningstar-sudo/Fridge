#!/usr/bin/env bash
# Starts the Postgres used by tests on localhost:55432 (idempotent).
set -e
docker start fridge-test-pg 2>/dev/null || docker run -d --name fridge-test-pg -p 55432:5432 -e POSTGRES_PASSWORD=postgres postgres:17
for _ in $(seq 1 60); do
  docker exec fridge-test-pg pg_isready -U postgres && exit 0
  sleep 1
done
echo "postgres did not become ready" >&2
exit 1
