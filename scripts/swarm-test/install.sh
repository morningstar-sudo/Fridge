#!/usr/bin/env bash
# Runs the real installer inside fsw1 of the test swarm (up.sh), with the locally built images instead of ghcr's.
# Usage: install.sh [installer options]   e.g. install.sh --admin alice
set -euo pipefail
cd "$(dirname "$0")/../.."

docker exec fsw1 rm -rf /root/inst
docker exec fsw1 mkdir -p /root/inst/deploy /opt/fridge
docker cp install.sh fsw1:/root/inst/install.sh
docker cp deploy/stack.yml fsw1:/root/inst/deploy/stack.yml
# .env keeps what it has, so seeding the image names makes the installer use the local images
docker exec fsw1 sh -c 'grep -q ^FRIDGE_IMAGE= /opt/fridge/.env 2>/dev/null || printf "FRIDGE_IMAGE=fridge:dev\nFRIDGE_JOB_IMAGE=fridge-job:dev\n" >> /opt/fridge/.env; chmod 600 /opt/fridge/.env'
docker exec fsw1 sh /root/inst/install.sh "$@"
