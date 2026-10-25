#!/usr/bin/env bash
# Browser smoke test of the web UI against a throwaway compose stack (project fridge-ui, own volumes,
# host ports 18443/18080/15432/19000). Playwright runs in a Docker image; it is a test tool, not a project dependency.
# Needs docker; run from anywhere. KEEP=1 leaves the stack up afterwards.
set -euo pipefail
cd "$(dirname "$0")/../.."

ADMIN_PW="${ADMIN_PW:-correct horse battery staple}"
SETUP_TOKEN="${SETUP_TOKEN:-$(openssl rand -hex 16)}"
READY="https://localhost:18443"
# The browser joins the stack network instead of using the host network: Chromium aborts requests with
# ERR_NETWORK_CHANGED when Docker adds a bridge for a new node mid-test.
BASE="${BASE:-https://fridge-ui-traefik:8443}"
ENVF=$(mktemp)
trap 'rm -f "$ENVF"' EXIT
cat >"$ENVF" <<EOF
FRIDGE_META_PASSWORD=$(openssl rand -hex 12)
FRIDGE_SECRET_KEY=$(openssl rand -base64 32)
FRIDGE_SETUP_TOKEN=$SETUP_TOKEN
FRIDGE_PUBLIC_HOSTS=
TZ=UTC
EOF
compose() { docker compose -f deploy/docker-compose.yml -f scripts/ui-smoke/compose.yml --env-file "$ENVF" --profile dev "$@"; }

# Whatever nodes this run creates are removed afterwards; nodes that exist now (a real stack) are left alone.
before_c=$(docker ps -aq --filter label=fridge.db_id | sort)
before_n=$(docker network ls --format '{{.Name}}' --filter name=fridge-node- | sort)
before_v=$(docker volume ls -q --filter name=fridge-postgres- | sort)
cleanup() {
	[ "${KEEP:-}" = 1 ] && return
	for c in $(comm -13 <(echo "$before_c") <(docker ps -aq --filter label=fridge.db_id | sort)); do docker rm -f "$c" >/dev/null 2>&1 || true; done
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	for n in $(comm -13 <(echo "$before_n") <(docker network ls --format '{{.Name}}' --filter name=fridge-node- | sort)); do docker network rm "$n" >/dev/null 2>&1 || true; done
	for v in $(comm -13 <(echo "$before_v") <(docker volume ls -q --filter name=fridge-postgres- | sort)); do docker volume rm "$v" >/dev/null 2>&1 || true; done
}
trap 'cleanup; rm -f "$ENVF"' EXIT

compose up -d --build
echo "waiting for $READY ..."
for _ in $(seq 1 90); do
	curl -ks -o /dev/null -w "%{http_code}" "$READY/readyz" 2>/dev/null | grep -q 200 && break
	sleep 2
done
curl -ks "$READY/readyz" | grep -q ok || { compose logs --tail 50 fridge; echo "stack did not become ready" >&2; exit 1; }

docker run --rm --network fridge-ui-net -v "$PWD/scripts/ui-smoke:/work" -w /work \
	-e BASE="$BASE" -e SETUP_TOKEN="$SETUP_TOKEN" -e ADMIN_PW="$ADMIN_PW" -e MINIO_ENDPOINT="http://minio:9000" \
	mcr.microsoft.com/playwright:v1.49.0-noble \
	sh -c 'npm i --no-save --no-audit --no-fund playwright@1.49.0 >/dev/null && node smoke.mjs'
