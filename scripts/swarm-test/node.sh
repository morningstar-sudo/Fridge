#!/usr/bin/env bash
# Starts one more docker:dind server (default fsw3) on the test network WITHOUT joining it to the swarm, and loads
# the given images into it. e2e.sh uses it to test adding a server with install.sh. down.sh removes fsw3 too.
# Usage: node.sh [name] [image ...]
set -euo pipefail
NAME=${1:-fsw3}; shift || true
docker rm -f -v "$NAME" >/dev/null 2>&1 || true
docker run -d --privileged --name "$NAME" --hostname "$NAME" --network fsw-net -e DOCKER_TLS_CERTDIR= docker:28-dind >/dev/null
for _ in $(seq 1 60); do docker exec "$NAME" docker info >/dev/null 2>&1 && break; sleep 1; done
for img in "$@"; do docker save "$img" | docker exec -i "$NAME" docker load >/dev/null; done
echo "$NAME is up (not in the swarm)"
