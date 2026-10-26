#!/usr/bin/env bash
# Removes the two-node test swarm made by up.sh (containers, their inner Docker state and the network).
set -uo pipefail
docker rm -f -v fsw1 fsw2 fsw3 >/dev/null 2>&1
docker network rm fsw-net >/dev/null 2>&1
echo "swarm test environment removed"
