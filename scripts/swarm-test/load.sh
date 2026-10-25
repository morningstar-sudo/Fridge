#!/usr/bin/env bash
# Copies images from the host daemon into both nodes of the test swarm. Usage: load.sh image ...
set -euo pipefail
for img in "$@"; do
	for n in fsw1 fsw2; do
		docker save "$img" | docker exec -i $n docker load >/dev/null
	done
	echo "loaded $img"
done
