#!/usr/bin/env bash
# Browser smoke test of the web UI against the real stack in the two-node test swarm (scripts/swarm-test/up.sh,
# with fridge:dev fridge-job:dev postgres:17 postgres:15 postgres:18 traefik:v3.5 cgr.dev/chainguard/minio loaded).
# It installs the stack with install.sh when it is not there yet, adds a MinIO service for the backup steps and runs
# Playwright (a test tool in a Docker image, not a project dependency) on the swarm's network.
# Needs docker; run from anywhere.
set -euo pipefail
cd "$(dirname "$0")/../.."

ADMIN_PW="${ADMIN_PW:-correct horse battery staple}"
D="docker exec -i fsw1 docker"

if ! $D stack services fridge >/dev/null 2>&1 || [ -z "$($D stack services fridge -q)" ]; then
	bash scripts/swarm-test/install.sh >/dev/null
fi
SETUP_TOKEN=$(docker exec fsw1 sh -c "sed -n 's/^FRIDGE_SETUP_TOKEN=//p' /opt/fridge/.env")

# S3 for the backup steps: on the egress overlay for the dump jobs and on the edge overlay for Fridge itself (a real S3 is outside)
if ! $D service inspect minio >/dev/null 2>&1; then
	$D service create -d --name minio --network fridge_egress --network fridge_edge --constraint node.role==manager \
		-e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin cgr.dev/chainguard/minio:latest server /data >/dev/null
	sleep 8
	$D service create -d --name minio-init --network fridge_egress --restart-condition none --mode replicated-job \
		-e MC_HOST_local=http://minioadmin:minioadmin@minio:9000 cgr.dev/chainguard/minio-client:latest mb -p local/fridge >/dev/null 2>&1 || true
	for _ in $(seq 1 60); do # the bucket must exist before the first test of the destination
		[ "$($D service ps minio-init --format '{{.CurrentState}}' 2>/dev/null | head -1 | cut -c1-8)" = Complete ] && break
		sleep 2
	done
fi

echo "waiting for https://fsw1:8443 ..."
for _ in $(seq 1 90); do
	docker run --rm --network fsw-net mcr.microsoft.com/playwright:v1.49.0-noble curl -ks https://fsw1:8443/readyz 2>/dev/null | grep -q ok && break
	sleep 2
done

docker run --rm --network fsw-net -v "$PWD/scripts/ui-smoke:/work" -w /work \
	-e BASE="https://fsw1:8443" -e SETUP_TOKEN="$SETUP_TOKEN" -e ADMIN_PW="$ADMIN_PW" -e MINIO_ENDPOINT="http://minio:9000" \
	mcr.microsoft.com/playwright:v1.49.0-noble \
	sh -c 'npm i --no-save --no-audit --no-fund playwright@1.49.0 >/dev/null && node smoke.mjs'
