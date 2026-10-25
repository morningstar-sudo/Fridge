#!/usr/bin/env bash
# Two-node Docker Swarm made of two docker:dind containers, so swarm tests never touch the host's Docker daemon.
#   fsw1 = manager (labels fridge.control, fridge.db), API on 127.0.0.1:23750; fsw2 = worker (label fridge.db).
# Usage: up.sh [image ...]   images are copied from the host daemon into both nodes (docker save | docker load).
set -euo pipefail

NET=fsw-net
IMG=docker:28-dind

if docker ps -a --format '{{.Names}}' | grep -qx fsw1; then echo "fsw1 exists: run down.sh first" >&2; exit 1; fi
docker network inspect $NET >/dev/null 2>&1 || docker network create $NET >/dev/null

run() { # <name> [extra docker run args]
	local name=$1; shift
	docker run -d --privileged --name "$name" --hostname "$name" --network $NET -e DOCKER_TLS_CERTDIR= "$@" $IMG >/dev/null
}
run fsw1 -p 127.0.0.1:23750:2375 -p 127.0.0.1:25432:5432 -p 127.0.0.1:28443:8443
run fsw2

wait_up() { for _ in $(seq 1 60); do docker exec "$1" docker info >/dev/null 2>&1 && return 0; sleep 1; done; echo "$1: dockerd did not start" >&2; return 1; }
wait_up fsw1
wait_up fsw2

ip1=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" fsw1)
docker exec fsw1 docker swarm init --advertise-addr "$ip1" >/dev/null
tok=$(docker exec fsw1 docker swarm join-token -q worker)
docker exec fsw2 docker swarm join --token "$tok" "$ip1:2377" >/dev/null
docker exec fsw1 docker node update --label-add fridge.control=true --label-add fridge.db=true fsw1 >/dev/null
docker exec fsw1 docker node update --label-add fridge.db=true fsw2 >/dev/null

for img in "$@"; do
	for n in fsw1 fsw2; do
		docker save "$img" | docker exec -i $n docker load >/dev/null
	done
	echo "loaded $img"
done
docker exec fsw1 docker node ls
echo "manager API: DOCKER_HOST=tcp://127.0.0.1:23750"
