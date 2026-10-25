#!/usr/bin/env bash
# Runs the swarm integration tests (-tags integration) inside the test swarm made by up.sh:
#   internal/app TestSwarm*  - node lifecycle, jobs, isolation, rotation
#   internal/api TestE2E*    - users, ownership and permissions through the HTTP API
# The tests need the Docker socket of a manager, the shared overlays, a metadata Postgres and an S3, so they are
# built into an image and run as one-shot services on the manager, like any other Fridge job.
# Needs on the host daemon: go, docker, and the images fridge:dev, fridge-job:dev, postgres:17/18 and
# cgr.dev/chainguard/minio (scripts/swarm-test/load.sh puts them into the nodes).
# Usage: gotest.sh [app|api]     (default: both)
set -euo pipefail
cd "$(dirname "$0")/../.."

WHICH=${1:-all}
M="docker exec fsw1 docker"
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"; $M service rm fridge-test-meta fridge-test-minio fridge-gotest >/dev/null 2>&1 || true' EXIT

CGO_ENABLED=0 GOOS=linux go test -c -tags integration -o "$OUT/app.test" ./internal/app/
CGO_ENABLED=0 GOOS=linux go test -c -tags integration -o "$OUT/api.test" ./internal/api/
cat >"$OUT/Dockerfile" <<'EOF'
FROM fridge-job:dev
USER root
COPY app.test api.test /
EOF
docker build -q -t fridge-gotest:dev "$OUT" >/dev/null
for n in fsw1 fsw2; do docker save fridge-gotest:dev | docker exec -i $n docker load >/dev/null; done

# the shared overlays exist before anything joins them (the stack creates them in a real install)
for net in fridge_edge fridge_egress; do
	$M network inspect $net >/dev/null 2>&1 || $M network create -d overlay --attachable --opt encrypted $net >/dev/null
done
$M service rm fridge-test-meta fridge-test-minio fridge-gotest >/dev/null 2>&1 || true

$M service create -d --name fridge-test-meta --network fridge_edge --constraint node.role==manager \
	-e POSTGRES_PASSWORD=postgres postgres:18 >/dev/null
# S3 lives on the egress overlay: that is the one network a dump job shares with it
$M service create -d --name fridge-test-minio --network fridge_egress --constraint node.role==manager \
	-e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
	cgr.dev/chainguard/minio:latest server /data >/dev/null

echo "waiting for the metadata Postgres ..."
for _ in $(seq 1 60); do
	$M exec "$($M ps -q --filter name=fridge-test-meta)" pg_isready -U postgres >/dev/null 2>&1 && break
	sleep 1
done
sleep 3

# run <binary> <-test.run pattern>
run() {
	$M service rm fridge-gotest >/dev/null 2>&1 || true
	$M service create -d --name fridge-gotest --mode replicated-job --restart-condition none \
		--constraint node.role==manager --network fridge_edge --network fridge_egress \
		--mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
		--entrypoint "$1" \
		-e FRIDGE_SERVER_IMAGE=fridge:dev -e FRIDGE_JOB_IMAGE=fridge-job:dev \
		-e FRIDGE_TEST_DATABASE_URL='postgres://postgres:postgres@fridge-test-meta:5432/postgres?sslmode=disable' \
		-e FRIDGE_IT_S3_ENDPOINT=http://fridge-test-minio:9000 -e FRIDGE_IT_S3_ACCESS=minioadmin -e FRIDGE_IT_S3_SECRET=minioadmin -e FRIDGE_IT_S3_BUCKET=fridge \
		fridge-gotest:dev -test.v -test.run "$2" -test.timeout 25m >/dev/null
	echo "running $1 $2 ..."
	local st=""
	for _ in $(seq 1 1400); do
		st=$($M service ps fridge-gotest --format '{{.CurrentState}}' 2>/dev/null | head -1)
		case "$st" in Complete*|Failed*|Rejected*) break ;; esac
		sleep 2
	done
	$M service logs --no-task-ids --raw fridge-gotest 2>&1 | tail -80
	case "$st" in Complete*) echo "PASS $1" ;; *) echo "FAIL $1 ($st)"; return 1 ;; esac
}

rc=0
case "$WHICH" in all|app) run /app.test TestSwarm || rc=1 ;; esac
case "$WHICH" in all|api) run /api.test TestE2E || rc=1 ;; esac
exit $rc
