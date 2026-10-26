#!/usr/bin/env bash
# Runs the real installer inside fsw1 (or $NODE) of the test swarm (up.sh), with the locally built images instead of ghcr's.
# Usage: [NODE=fsw3] install.sh [installer options]   e.g. install.sh --admin alice
set -euo pipefail
cd "$(dirname "$0")/../.."

NODE=${NODE:-fsw1}
docker exec "$NODE" rm -rf /root/inst
docker exec "$NODE" mkdir -p /root/inst/deploy /opt/fridge
docker cp install.sh "$NODE:/root/inst/install.sh"
docker cp deploy/stack.yml "$NODE:/root/inst/deploy/stack.yml"
# .env keeps what it has, so seeding the image names makes the installer use the local images (not needed to join)
case "${1:-}" in --join|--add-node) ;; *)
	docker exec "$NODE" sh -c 'grep -q ^FRIDGE_IMAGE= /opt/fridge/.env 2>/dev/null || printf "FRIDGE_IMAGE=fridge:dev\nFRIDGE_JOB_IMAGE=fridge-job:dev\n" >> /opt/fridge/.env; chmod 600 /opt/fridge/.env' ;;
esac
docker exec "$NODE" sh /root/inst/install.sh "$@"
