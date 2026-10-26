#!/bin/sh
# Fridge installer: Postgres (and more) nodes on Docker Swarm, with TLS routing, backups to S3 and a web UI.
#
#   curl -fsSL https://raw.githubusercontent.com/morningstar-sudo/Fridge/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --admin alice --hosts fridge.example.com
#
# Run it on the server that becomes the swarm manager. It starts a swarm if there is none, labels this server,
# writes .env with fresh secrets (only when there is none: an existing .env is never regenerated) and deploys the
# stack. Running it again upgrades. Needs Docker; curl is only needed to download stack.yml (and to wait for readiness).
#
#   --admin NAME        create the first admin NAME with a random password (printed once)
#   --hosts A,B         names/IPs the web UI certificate must cover (default: this server's hostname and address)
#   --version TAG       image tag to run (default: latest); remembered in .env
#   --dir DIR           where .env and stack.yml live (default: /opt/fridge, or ~/fridge without root)
#   --advertise-addr A  address other servers use to join, when `docker swarm init` cannot pick one
#   --uninstall         remove the stack; databases, their volumes and the metadata stay
#   --purge             with --uninstall: also remove every database node, all volumes and --dir
#   --yes               do not ask before --purge
#
# Adding a server (nothing is installed on it but Docker; databases may be placed on it as soon as it joins):
#   --add-node            on the manager: print the one command to run on the new server
#   --add-node USER@HOST  on the manager: run that command on the new server over SSH and wait until it is ready
#   --join ADDR --token T on the new server: join the swarm of the manager at ADDR (what --add-node prints)
# The new server needs 2377/tcp (to the manager), 7946/tcp+udp, 4789/udp and ESP (IP protocol 50) open between servers.
set -eu

REPO=morningstar-sudo/Fridge
STACK=fridge
ADMIN="" HOSTS="" VERSION="" DIR="" ADDR="" UNINSTALL=0 PURGE=0 YES=0 ADD_NODE=0 ADD_TARGET="" JOIN="" TOKEN=""

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--admin) ADMIN=${2:?--admin needs a name}; shift 2 ;;
	--hosts) HOSTS=${2:?--hosts needs a list}; shift 2 ;;
	--version) VERSION=${2:?--version needs a tag}; shift 2 ;;
	--dir) DIR=${2:?--dir needs a path}; shift 2 ;;
	--advertise-addr) ADDR=${2:?--advertise-addr needs an address}; shift 2 ;;
	--uninstall) UNINSTALL=1; shift ;;
	--purge) PURGE=1; shift ;;
	--yes) YES=1; shift ;;
	--add-node) ADD_NODE=1; case "${2:-}" in ""|-*) shift ;; *) ADD_TARGET=$2; shift 2 ;; esac ;;
	--join) JOIN=${2:?--join needs the manager address}; shift 2 ;;
	--token) TOKEN=${2:?--token needs the worker join token}; shift 2 ;;
	-h|--help) sed -n '2,24p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown option $1 (see --help)" ;;
	esac
done

command -v docker >/dev/null 2>&1 || die "docker is not installed (https://docs.docker.com/engine/install/)"
docker info >/dev/null 2>&1 || die "cannot talk to the Docker daemon: is it running, and are you in the docker group (or root)?"
if [ -z "$DIR" ]; then
	if [ "$(id -u)" = 0 ]; then DIR=/opt/fridge; else DIR=$HOME/fridge; fi
fi
ENVF=$DIR/.env

swarm_state() { docker info --format '{{.Swarm.LocalNodeState}}'; }
rand_hex() { head -c "$1" /dev/urandom | od -An -vtx1 | tr -d ' \n'; }
getenv() { sed -n "s/^$1=//p" "$ENVF" 2>/dev/null | head -n 1; }
# setenv KEY VALUE: add KEY only when .env has none
setenv() { grep -q "^$1=" "$ENVF" 2>/dev/null || printf '%s=%s\n' "$1" "$2" >>"$ENVF"; }
# putenv KEY VALUE: add or replace KEY
putenv() {
	if grep -q "^$1=" "$ENVF" 2>/dev/null; then
		grep -v "^$1=" "$ENVF" >"$ENVF.new" || true
		printf '%s=%s\n' "$1" "$2" >>"$ENVF.new"
		chmod 600 "$ENVF.new"; mv "$ENVF.new" "$ENVF"
	else
		printf '%s=%s\n' "$1" "$2" >>"$ENVF"
	fi
}

# all replicas of every service of the stack are up
stack_up() {
	n=$(docker stack services "$STACK" --format '{{.Replicas}}' 2>/dev/null | wc -l | tr -d ' ')
	[ "$n" -ge 3 ] || return 1
	docker stack services "$STACK" --format '{{.Replicas}}' | while read -r r; do
		up=${r%%/*}; want=${r#*/}; want=${want%% *}
		[ "$up" = "$want" ] && [ "$up" != 0 ] || exit 1
	done
}

# the one-shot volume cleanup has finished on every server
purge_done() {
	out=$(docker service ps fridge-purge --format '{{.CurrentState}}' 2>/dev/null) || return 1
	[ -n "$out" ] && ! printf '%s\n' "$out" | grep -qv '^Complete'
}

wait_for() { # <seconds> <what> <command...>
	t=$1; what=$2; shift 2
	i=0
	while [ "$i" -lt "$t" ]; do
		if "$@"; then return 0; fi
		i=$((i + 2)); sleep 2
	done
	die "timed out waiting for $what"
}

# ------------------------------------------------------------------ add a server
# On the new server: join the swarm. Traefik (a global service) starts there by itself and the server may hold databases
# from now on (docker node update --label-add fridge.db=false <node> keeps them off).
if [ -n "$JOIN" ]; then
	[ -n "$TOKEN" ] || die "--join needs --token (docker swarm join-token worker, on the manager)"
	case "$JOIN" in *:*) ;; *) JOIN=$JOIN:2377 ;; esac
	case "$(swarm_state)" in
	active) say "This server is already in a swarm; nothing to do."; exit 0 ;;
	inactive) ;;
	*) die "this server's swarm state is '$(swarm_state)'; fix it first (docker swarm leave --force starts over)" ;;
	esac
	say "Joining the swarm at $JOIN ..."
	if [ -n "$ADDR" ]; then set -- --advertise-addr "$ADDR"; else set --; fi
	docker swarm join "$@" --token "$TOKEN" "$JOIN" >/dev/null || die "could not join $JOIN: check that 2377/tcp, 7946/tcp+udp, 4789/udp and ESP are open between the servers (and --advertise-addr if this server has several addresses)"
	say "Joined. Traefik starts on this server within a minute; databases can be placed here from now on."
	say "Images are pulled on demand (and by Images > Pull). If the images are private: docker login ghcr.io on this server."
	exit 0
fi

# On the manager: print (or, over SSH, run) the command that makes another server join.
if [ "$ADD_NODE" = 1 ]; then
	[ "$(swarm_state)" = active ] && [ "$(docker info --format '{{.Swarm.ControlAvailable}}')" = true ] || die "--add-node runs on the swarm manager"
	WTOKEN=$(docker swarm join-token -q worker)
	MGR=$(docker info --format '{{.Swarm.NodeAddr}}'):2377
	JOIN_ARGS="--join $MGR --token $WTOKEN"
	if [ -z "$ADD_TARGET" ]; then
		say "On the new server (it needs Docker, and 2377/tcp to this server, 7946/tcp+udp, 4789/udp and ESP between servers):"
		say ""
		say "  curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sh -s -- $JOIN_ARGS"
		say ""
		say "The token is a secret: whoever has it can join the swarm. Revoke it with: docker swarm join-token --rotate worker"
		say "Or let this server do it over SSH:  sh install.sh --add-node USER@HOST"
		exit 0
	fi
	SCRIPT=$0
	if [ ! -f "$SCRIPT" ]; then # piped from curl: fetch the installer again to hand it to the new server
		command -v curl >/dev/null 2>&1 || die "curl is needed to fetch the installer for $ADD_TARGET"
		SCRIPT=$(mktemp)
		curl -fsSL "https://raw.githubusercontent.com/$REPO/main/install.sh" -o "$SCRIPT" || die "could not download install.sh"
	fi
	BEFORE=$(docker node ls --format '{{.Status}}' | grep -c '^Ready$' || true)
	say "Joining $ADD_TARGET over SSH ..."
	# shellcheck disable=SC2086
	if [ -n "$ADDR" ]; then JOIN_ARGS="$JOIN_ARGS --advertise-addr $ADDR"; fi
	ssh ${SSH_OPTS:-} "$ADD_TARGET" sh -s -- $JOIN_ARGS <"$SCRIPT" || die "the installer failed on $ADD_TARGET"
	node_up() { [ "$(docker node ls --format '{{.Status}}' | grep -c '^Ready$')" -gt "$BEFORE" ]; }
	wait_for 120 "the new server to show up as Ready in docker node ls" node_up
	say "$ADD_TARGET is in the swarm. docker node ls:"
	docker node ls
	exit 0
fi

# ------------------------------------------------------------------ uninstall
if [ "$UNINSTALL" = 1 ]; then
	[ "$(swarm_state)" = active ] || die "this server is not in a swarm: nothing to uninstall"
	if [ "$PURGE" = 1 ] && [ "$YES" != 1 ]; then
		say "--purge deletes every database node, all their data and the metadata. This cannot be undone."
		if [ -t 0 ]; then
			printf 'Type "delete everything" to continue: '
			read -r answer
			[ "$answer" = "delete everything" ] || die "aborted"
		else
			die "--purge needs --yes when there is no terminal"
		fi
	fi
	say "Removing the stack ..."
	docker stack rm "$STACK" >/dev/null 2>&1 || true
	wait_for 120 "the stack to go away" sh -c "! docker stack services $STACK 2>/dev/null | grep -q ."
	if [ "$PURGE" = 1 ]; then
		say "Removing database nodes ..."
		for s in $(docker service ls --format '{{.Name}}' | grep -E '^fridge-(db|edge)-' || true); do docker service rm "$s" >/dev/null; done
		sleep 5
		for n in $(docker network ls --format '{{.Name}}' | grep -E '^fridge_edge$|^fridge_egress$|^fridge_db$|^fridge_meta$' || true); do
			docker network rm "$n" >/dev/null 2>&1 || true
		done
		say "Removing volumes on every server ..."
		docker service rm fridge-purge >/dev/null 2>&1 || true
		docker service create -d --name fridge-purge --mode global-job --restart-condition none \
			--mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
			docker:cli sh -c 'for v in $(docker volume ls -q | grep -E "^fridge-postgres-|^fridge_fridge-(meta|data)$"); do docker volume rm "$v"; done' >/dev/null
		wait_for 300 "the volume cleanup (it pulls docker:cli on every server)" purge_done
		docker service rm fridge-purge >/dev/null 2>&1 || true
		rm -rf "$DIR"
		say "Fridge and all of its data are gone."
	else
		say "Fridge is stopped. Databases keep running with their volumes; the metadata and .env in $DIR stay."
		say "Install again to bring the web UI back, or run with --uninstall --purge to delete everything."
	fi
	exit 0
fi

# ------------------------------------------------------------------ install
case "$(swarm_state)" in
active)
	[ "$(docker info --format '{{.Swarm.ControlAvailable}}')" = true ] || die "this server is a swarm worker: run the installer on a manager"
	;;
inactive)
	say "Starting a swarm on this server ..."
	if [ -n "$ADDR" ]; then docker swarm init --advertise-addr "$ADDR" >/dev/null; else
		docker swarm init >/dev/null 2>&1 || die "docker swarm init failed (often: several addresses). Re-run with --advertise-addr <ip of this server>"
	fi
	;;
*) die "this server's swarm state is '$(swarm_state)'; fix it first (docker swarm leave --force starts over)" ;;
esac

NODE=$(docker info --format '{{.Swarm.NodeID}}')
docker node update --label-add fridge.control=true --label-add fridge.db=true "$NODE" >/dev/null

mkdir -p "$DIR"
chmod 700 "$DIR"
[ -f "$ENVF" ] || { : >"$ENVF"; chmod 600 "$ENVF"; }

# stack.yml: the copy next to this script when it runs from a checkout, otherwise the one on GitHub
REF=main
VERSION=${VERSION#v} # tags are v1.2.3, images 1.2.3
[ -n "$VERSION" ] && [ "$VERSION" != latest ] && REF=v$VERSION
HERE=$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)
if [ -n "$HERE" ] && [ -f "$HERE/deploy/stack.yml" ]; then
	cp "$HERE/deploy/stack.yml" "$DIR/stack.yml"
else
	command -v curl >/dev/null 2>&1 || die "curl is needed to download stack.yml"
	curl -fsSL "https://raw.githubusercontent.com/$REPO/$REF/deploy/stack.yml" -o "$DIR/stack.yml" || die "could not download stack.yml ($REF)"
fi

# the installer stays next to .env: --add-node, --uninstall and upgrades are run from there later
if [ -f "$0" ]; then
	[ "$0" -ef "$DIR/install.sh" ] || cp "$0" "$DIR/install.sh"
elif command -v curl >/dev/null 2>&1; then
	curl -fsSL "https://raw.githubusercontent.com/$REPO/$REF/install.sh" -o "$DIR/install.sh" || true
fi

# fresh secrets only for what .env lacks: FRIDGE_SECRET_KEY encrypts every stored password, so it is never regenerated
setenv FRIDGE_META_PASSWORD "$(rand_hex 16)"
setenv FRIDGE_SECRET_KEY "$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
setenv FRIDGE_PROVIDER_TOKEN "$(rand_hex 24)"
setenv FRIDGE_SETUP_TOKEN "$(rand_hex 16)"
setenv TZ "${TZ:-UTC}"
if [ -n "$VERSION" ]; then
	putenv FRIDGE_IMAGE "ghcr.io/morningstar-sudo/fridge:$VERSION"
	putenv FRIDGE_JOB_IMAGE "ghcr.io/morningstar-sudo/fridge-job:$VERSION"
else
	setenv FRIDGE_IMAGE "ghcr.io/morningstar-sudo/fridge:latest"
	setenv FRIDGE_JOB_IMAGE "ghcr.io/morningstar-sudo/fridge-job:latest"
fi
if [ -n "$HOSTS" ]; then
	putenv FRIDGE_PUBLIC_HOSTS "$HOSTS"
elif [ -z "$(getenv FRIDGE_PUBLIC_HOSTS)" ]; then
	h=$(hostname 2>/dev/null || true)
	ip=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n 1 || true)
	list=$h
	[ -n "$ip" ] && list=${list:+$list,}$ip
	putenv FRIDGE_PUBLIC_HOSTS "$list"
fi

say "Deploying the stack ..."
(
	set -a
	# shellcheck disable=SC1090
	. "$ENVF"
	set +a
	docker stack deploy --with-registry-auth -c "$DIR/stack.yml" "$STACK"
)
wait_for 600 "the services to start (docker stack ps $STACK --no-trunc shows why not)" stack_up
if command -v curl >/dev/null 2>&1; then
	wait_for 120 "Fridge to answer on https://localhost:8443" sh -c 'curl -ks https://localhost:8443/readyz | grep -q ok'
fi

TOKEN=$(getenv FRIDGE_SETUP_TOKEN)
IMAGE=$(getenv FRIDGE_IMAGE)
CREATED=""
if [ -n "$ADMIN" ]; then
	PW=$(rand_hex 12)
	i=0
	while [ "$i" -lt 60 ]; do # Traefik may need a few more seconds to pick up Fridge's routes
		if ERR=$(printf '%s\n' "$PW" | docker run --rm -i --network host -e FRIDGE_URL=https://localhost:8443 -e FRIDGE_CONFIG_DIR=/tmp/fridge \
			--entrypoint /fridge "$IMAGE" setup --setup-token "$TOKEN" --user "$ADMIN" --password-stdin --yes 2>&1); then
			CREATED=1; break
		fi
		case "$ERR" in *token*|*already*|*account*|*password*) break ;; esac # a real refusal: retrying will not help
		i=$((i + 1)); sleep 2
	done
	if [ -z "$CREATED" ]; then
		say "Could not create the admin: $ERR"
		say "Open the web UI to set one up, or run: fridge setup --setup-token <FRIDGE_SETUP_TOKEN in $ENVF> --user $ADMIN"
	fi
fi

say ""
say "Fridge is running."
say "  Web UI    https://$(getenv FRIDGE_PUBLIC_HOSTS | cut -d, -f1):8443   (the certificate is signed by Fridge's own CA: the CLI asks to trust it on first login)"
say "  Databases clients connect to <the database's domain> on any server of the swarm, TLS required: 5432 PostgreSQL, 3306 MySQL/MariaDB, 27017 MongoDB, 6379 Redis"
if [ -n "$CREATED" ]; then
	say "  Admin     $ADMIN   password: $PW   (shown once: change it in the web UI)"
else
	say "  First admin: open the web UI and use the setup token from $ENVF (FRIDGE_SETUP_TOKEN), or re-run with --admin NAME"
fi
say "  Config    $ENVF (keep FRIDGE_SECRET_KEY safe: without it the stored passwords are unreadable)"
say ""
say "Open ports on the server's firewall: 8443 for the web UI (limited by Settings > IP allowlist) and the ports of the engines you use (5432, 3306, 27017, 6379)."
say "The database ports are not filtered by Fridge: the swarm routing mesh hides client addresses, so restrict it with the firewall."
say "To add a server (nothing but Docker is needed on it):"
say "  sh $DIR/install.sh --add-node               # prints the one command to run on the new server"
say "  sh $DIR/install.sh --add-node USER@HOST     # or does it over SSH; databases can be placed on it as soon as it joins"
