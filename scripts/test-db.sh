#!/usr/bin/env bash
# Starts the Postgres 18 used by tests on localhost:55432 (idempotent; recreates a container on another image).
set -e
img=postgres:18
if docker inspect fridge-test-pg >/dev/null 2>&1 && [ "$(docker inspect -f '{{.Config.Image}}' fridge-test-pg)" != "$img" ]; then
  docker rm -f fridge-test-pg
fi
docker start fridge-test-pg 2>/dev/null || docker run -d --name fridge-test-pg -p 55432:5432 -e POSTGRES_PASSWORD=postgres $img
for _ in $(seq 1 60); do
  docker exec fridge-test-pg pg_isready -U postgres && exit 0
  sleep 1
done
echo "postgres did not become ready" >&2
exit 1
