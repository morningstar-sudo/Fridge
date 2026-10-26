#!/usr/bin/env bash
# Copies images from the host daemon into every server of the test swarm. Usage: load.sh image ...
set -euo pipefail
NODES=$(docker ps --format '{{.Names}}' | grep -E '^fsw[0-9]+$' | sort)
for img in "$@"; do
	for n in $NODES; do
		docker save "$img" | docker exec -i "$n" docker load >/dev/null
	done
	echo "loaded $img"
done
