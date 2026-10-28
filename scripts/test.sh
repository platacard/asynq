#!/usr/bin/env bash
#
# Runs the tests against a throwaway redis that is started for the run and
# removed afterwards, so the tests never touch a redis used for development.
#
# Usage:
#   scripts/test.sh [--cluster] [-- <extra go test flags>]
#
#   --cluster   run against redis clusters with 3 masters instead of standalone redis
#
# Examples:
#   scripts/test.sh
#   scripts/test.sh -- -run KeyPrefix -v
#   scripts/test.sh --cluster
#
# Environment:
#   REDIS_RUNNER    "docker" or "local" (redis-server from PATH).
#                   Default: docker if the docker daemon is available, otherwise local.
#   REDIS_IMAGE     docker image to use. Default: redis:7 (the same as in CI).
#   REDIS_PORT      port of the first redis node: the nodes use consecutive ports.
#                   Default: the first free ports from 56379 (standalone) or 36379 (cluster).
#   TEST_WORKERS    number of parallel workers for the packages that use redis, each with
#                   its own standalone redis or cluster. Default: 6. With -run, the tests
#                   run with a single worker.
#   GO_TEST_FLAGS   flags for go test. Default: "-race -count=1".
#   MODULES         go modules to test. Default: ". x".

set -euo pipefail

cd "$(dirname "$0")/.."

cluster=false
extra_args=()
while [[ $# -gt 0 ]]; do
	case "$1" in
	--cluster)
		cluster=true
		shift
		;;
	--)
		shift
		extra_args=("$@")
		break
		;;
	*)
		echo "unknown argument: $1" >&2
		exit 2
		;;
	esac
done

image=${REDIS_IMAGE:-redis:7}
read -r -a go_test_flags <<<"${GO_TEST_FLAGS:--race -count=1}"
read -r -a modules <<<"${MODULES:-. x}"

log() { echo "==> $*" >&2; }

# Most of the tests that use redis wait for timers, and the tests of a package
# run one by one, since they share a redis database. So the tests are run by
# several workers in parallel, each against its own redis node or cluster, so
# that the tests of different workers never see each other's data or pub/sub
# messages.
workers=${TEST_WORKERS:-6}
for arg in ${extra_args[@]+"${extra_args[@]}"}; do
	case "$arg" in
	-run | -run=* | --run | --run=* | -test.run | -test.run=*) workers=1 ;;
	esac
done

if [[ -z "${REDIS_RUNNER:-}" ]]; then
	if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
		REDIS_RUNNER=docker
	else
		REDIS_RUNNER=local
	fi
fi
case "$REDIS_RUNNER" in
docker) ;;
local)
	for bin in redis-server redis-cli; do
		if ! command -v "$bin" >/dev/null 2>&1; then
			echo "$bin not found: install redis or run with REDIS_RUNNER=docker" >&2
			exit 1
		fi
	done
	;;
*)
	echo "unknown REDIS_RUNNER: $REDIS_RUNNER (want docker or local)" >&2
	exit 2
	;;
esac

port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# Finds the first port from $1 such that $2 consecutive ports are free.
find_free_ports() {
	local port=$1 count=$2 i
	while true; do
		for ((i = 0; i < count; i++)); do
			if port_in_use $((port + i)); then
				port=$((port + i + 1))
				continue 2
			fi
		done
		echo "$port"
		return
	done
}

name="asynq-test-redis-$$"
workdir=""
node_ports=()

# Runs redis-cli where redis runs.
rcli() {
	if [[ "$REDIS_RUNNER" == docker ]]; then
		docker exec "$name" redis-cli "$@"
	else
		redis-cli "$@"
	fi
}

# Reports whether the redis on port $1 is the node started by this script and
# not another redis that took the port first, so that the tests never flush the
# data of another redis.
is_own_node() {
	local p=$1
	if [[ "$REDIS_RUNNER" == docker ]]; then
		# Ports published by the container cannot be taken by another process.
		rcli -p "$p" ping
	else
		[[ "$(redis-cli -p "$p" config get pidfile 2>/dev/null | tail -n 1)" == "$workdir/redis-$p.pid" ]]
	fi
}

# The command that starts a redis node in the background. Cluster nodes
# announce 127.0.0.1, so that the tests can reach them from the host: in docker,
# the container ports are published to the same host ports.
redis_server_cmd() {
	local p=$1 dir=$2
	local cmd="redis-server --port $p --save '' --appendonly no --daemonize yes --dir $dir"
	cmd+=" --logfile $dir/redis-$p.log --pidfile $dir/redis-$p.pid"
	if $cluster; then
		cmd+=" --cluster-enabled yes --cluster-config-file $dir/nodes-$p.conf --cluster-announce-ip 127.0.0.1"
	fi
	echo "$cmd"
}

wait_until() {
	local desc=$1
	shift
	for _ in $(seq 1 100); do
		if "$@" >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.1
	done
	echo "timed out waiting for $desc" >&2
	return 1
}

# Starts the redis nodes on ports $port..$port+$nodes-1.
start_redis() {
	local i p
	node_ports=()
	for ((i = 0; i < nodes; i++)); do
		node_ports+=($((port + i)))
	done
	if [[ "$REDIS_RUNNER" == docker ]]; then
		local publish=() start=""
		for p in "${node_ports[@]}"; do
			publish+=(-p "127.0.0.1:$p:$p")
			start+="$(redis_server_cmd "$p" /data) && "
		done
		docker run -d --rm --name "$name" "${publish[@]}" "$image" \
			sh -c "${start} exec sleep infinity" >/dev/null || return 1
	else
		workdir=$(mktemp -d "${TMPDIR:-/tmp}/asynq-test-redis.XXXXXX")
		for p in "${node_ports[@]}"; do
			eval "$(redis_server_cmd "$p" "$workdir")" || return 1
		done
	fi
	for p in "${node_ports[@]}"; do
		wait_until "redis on port $p" is_own_node "$p" || return 1
	done
}

stop_redis() {
	if [[ "$REDIS_RUNNER" == docker ]]; then
		docker rm -f "$name" >/dev/null 2>&1 || true
	else
		local p
		for p in ${node_ports[@]+"${node_ports[@]}"}; do
			if is_own_node "$p" >/dev/null 2>&1; then
				redis-cli -p "$p" shutdown nosave >/dev/null 2>&1 || true
			fi
		done
		if [[ -n "$workdir" ]]; then
			rm -rf "$workdir"
		fi
		workdir=""
	fi
}

logdir=$(mktemp -d "${TMPDIR:-/tmp}/asynq-test-logs.XXXXXX")

cleanup() {
	log "Stopping redis"
	stop_redis
	rm -rf "$logdir"
}
trap cleanup EXIT

if $cluster; then
	nodes_per_worker=3
	# The cluster bus ports are 10000 higher, below the ports of standalone nodes.
	first_port=36379
else
	nodes_per_worker=1
	first_port=56379
fi
nodes=$((workers * nodes_per_worker))
# The ports are checked before redis starts, so another process may take them
# in between, e.g. a parallel run of this script. Then other ports are tried.
attempts=5
for ((attempt = 1; ; attempt++)); do
	port=${REDIS_PORT:-$(find_free_ports "$first_port" "$nodes")}
	if $cluster; then
		log "Starting redis ($REDIS_RUNNER, $workers clusters of $nodes_per_worker nodes on ports $port-$((port + nodes - 1)))"
	elif [[ $nodes -gt 1 ]]; then
		log "Starting redis ($REDIS_RUNNER, $nodes standalone nodes on ports $port-$((port + nodes - 1)))"
	else
		log "Starting redis ($REDIS_RUNNER, standalone on port $port)"
	fi
	if start_redis; then
		break
	fi
	stop_redis
	if [[ -n "${REDIS_PORT:-}" || $attempt -ge $attempts ]]; then
		echo "could not start redis" >&2
		exit 1
	fi
	first_port=$((port + nodes))
done

# Prints the addresses of the cluster nodes of worker $1, separated by commas.
cluster_addrs() {
	local w=$1 i addrs=()
	for ((i = 0; i < nodes_per_worker; i++)); do
		addrs+=("127.0.0.1:${node_ports[w * nodes_per_worker + i]}")
	done
	(
		IFS=,
		echo "${addrs[*]}"
	)
}

# Creates the cluster of worker $1 from its nodes.
create_cluster() {
	local w=$1 nodes_list
	nodes_list=$(cluster_addrs "$w")
	# shellcheck disable=SC2086 # split the addresses into arguments
	rcli --cluster create ${nodes_list//,/ } --cluster-replicas 0 --cluster-yes >/dev/null || return 1
	cluster_ok() { rcli -p "${node_ports[w * nodes_per_worker]}" cluster info | grep -q 'cluster_state:ok'; }
	wait_until "redis cluster $((w + 1))" cluster_ok
}

if $cluster; then
	# Creating a cluster takes a few seconds, so the clusters are created in parallel.
	pids=()
	for ((w = 0; w < workers; w++)); do
		create_cluster "$w" &
		pids+=($!)
	done
	for pid in "${pids[@]}"; do
		wait "$pid" || {
			echo "could not create redis clusters" >&2
			exit 1
		}
	done
fi

# Sets the go test flags that make the tests use the redis of worker $1:
# go_flags go before the packages, and redis_flags after them.
set_worker_flags() {
	local w=$1
	if $cluster; then
		# The tests flush the whole cluster at the start of each test, so the
		# packages of a worker must not run in parallel.
		go_flags=(-p 1)
		redis_flags=(-redis_cluster "-redis_cluster_addrs=$(cluster_addrs "$w")")
	else
		go_flags=()
		redis_flags=("-redis_addr=127.0.0.1:${node_ports[w]}")
	fi
}

# Runs go test for the packages that use redis against the redis of worker $2,
# with extra flags $3....
test_redis_pkgs() {
	local module=$1 w=$2
	shift 2
	set_worker_flags "$w"
	(cd "$module" && go test ${go_test_flags[@]+"${go_test_flags[@]}"} ${go_flags[@]+"${go_flags[@]}"} \
		"${redis_pkgs[@]}" "$@" ${extra_args[@]+"${extra_args[@]}"} "${redis_flags[@]}")
}

# Prints the index of the next group of tests to run, or fails if there are no more groups.
next_group() {
	local n
	while ! mkdir "$logdir/lock" 2>/dev/null; do
		sleep 0.05
	done
	n=$(<"$logdir/next")
	echo $((n + 1)) >|"$logdir/next"
	rmdir "$logdir/lock"
	[[ $n -lt ${#groups[@]} ]] || return 1
	echo "$n"
}

# Runs the groups of tests from the queue against the redis of worker $2 until the queue is empty.
worker() {
	local module=$1 w=$2 g
	while g=$(next_group); do
		if [[ -z "${groups[g]}" ]]; then
			continue
		fi
		if test_redis_pkgs "$module" "$w" -run "^(${groups[g]})\$" >|"$logdir/group-$g.log" 2>&1; then
			echo 0 >|"$logdir/group-$g.status"
		else
			echo 1 >|"$logdir/group-$g.status"
		fi
	done
}

# Prints the names of the tests in the given directories that check for leaked
# goroutines with goleak.
goleak_tests() {
	local dir files=()
	for dir in "$@"; do
		files+=("$dir"/*_test.go)
	done
	awk '
		/^func (Test|Example|Fuzz)/ { name = $2; sub(/\(.*/, "", name) }
		/goleak\.Verify/ && name != "" { print name }
	' "${files[@]}" | sort -u
}

# Tests the packages that use redis with $workers parallel workers. The tests
# are split into groups by a hash of their names, and the workers take the
# groups from a queue. There are more groups than workers, so that the groups
# with slow tests are spread over the workers. A test with the same name in
# several packages is in the same group.
#
# The tests that check for leaked goroutines are in a separate group: in a
# group with other tests, they would fail because of the goroutines that other
# tests leave running for a while after they end.
test_redis_pkgs_in_parallel() {
	local module=$1 list leak_tests g name w pid n=$((workers * 2)) failed=0 start=$SECONDS
	if ! list=$(cd "$module" && go test ${go_test_flags[@]+"${go_test_flags[@]}"} -list '.*' "${redis_pkgs[@]}" 2>&1); then
		echo "$list" >&2
		return 1
	fi
	# With few tests, starting the groups takes longer than running the tests one by one.
	if [[ $(grep -cE '^(Test|Example|Fuzz)' <<<"$list") -le $((workers * 4)) ]]; then
		test_redis_pkgs "$module" 0
		return
	fi
	leak_tests=$(goleak_tests "${redis_dirs[@]}" | tr '\n' ' ')
	groups=()
	for ((g = 0; g <= n; g++)); do
		groups+=("")
	done
	while read -r g name; do
		groups[g]+="${groups[g]:+|}$name"
	done < <(grep -E '^(Test|Example|Fuzz)' <<<"$list" | sort -u | awk -v n="$n" -v leak_tests="$leak_tests" '
		BEGIN {
			for (i = 32; i < 127; i++) ord[sprintf("%c", i)] = i
			split(leak_tests, names, " ")
			for (i in names) leak[names[i]] = 1
		}
		$0 in leak { print n, $0; next }
		{
			h = 0
			for (i = 1; i <= length($0); i++) h = (h * 31 + ord[substr($0, i, 1)]) % 1000003
			print h % n, $0
		}')
	n=${#groups[@]}
	log "Running the tests in $n groups with $workers workers"

	echo 0 >|"$logdir/next"
	rm -f "$logdir"/group-*
	local pids=()
	for ((w = 0; w < workers; w++)); do
		worker "$module" "$w" &
		pids+=($!)
	done
	for pid in "${pids[@]}"; do
		wait "$pid" || failed=1
	done

	for ((g = 0; g < n; g++)); do
		if [[ -f "$logdir/group-$g.log" ]]; then
			log "Group $((g + 1))/$n"
			cat "$logdir/group-$g.log"
			if [[ "$(<"$logdir/group-$g.status")" != 0 ]]; then
				failed=1
			fi
		fi
	done
	log "Finished the groups in $((SECONDS - start))s"
	return $failed
}

# Only some packages define the redis flags, and go test fails for the others
# if they are passed, so the packages are tested in two groups.
status=0
for module in "${modules[@]}"; do
	redis_pkgs=()
	redis_dirs=()
	other_pkgs=()
	while read -r pkg dir; do
		if grep -qs '"redis_addr"' "$dir"/*_test.go; then
			redis_pkgs+=("$pkg")
			redis_dirs+=("$dir")
		else
			other_pkgs+=("$pkg")
		fi
	done < <(cd "$module" && go list -f '{{.ImportPath}} {{.Dir}}' ./...)

	if [[ ${#other_pkgs[@]} -gt 0 ]]; then
		log "Testing packages without redis in module $module"
		(cd "$module" && go test ${go_test_flags[@]+"${go_test_flags[@]}"} "${other_pkgs[@]}" ${extra_args[@]+"${extra_args[@]}"}) || status=1
	fi
	if [[ ${#redis_pkgs[@]} -gt 0 && $workers -gt 1 ]]; then
		log "Testing packages with redis in module $module"
		test_redis_pkgs_in_parallel "$module" || status=1
	elif [[ ${#redis_pkgs[@]} -gt 0 ]]; then
		log "Testing packages with redis in module $module"
		test_redis_pkgs "$module" 0 || status=1
	fi
done

if [[ $status -eq 0 ]]; then
	log "All tests passed"
else
	log "Some tests failed"
fi
exit $status
