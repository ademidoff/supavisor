#!/usr/bin/env bash
#
# Does supavisor leak zombies when it runs as PID 1?
#
# PID 1 inherits every orphan. A program that backgrounds a grandchild and then
# exits leaves that grandchild reparented to supavisor, and when it dies somebody
# has to wait for it or it stays a zombie. This runs exactly that shape in a
# container and counts what is left behind.
#
#   ./probes/pid1-zombies.sh          supavisor as PID 1, the case under test
#   ./probes/pid1-zombies.sh --init   control: docker-init as PID 1 instead
#
# See probes/README.md for what a correct run looks like, and for why the process
# table matters and not only the count.
set -euo pipefail

USE_INIT=""
CONTAINER="supavisor-pid1-check"
IMAGE="${IMAGE:-alpine:3}"
SAMPLES="${SAMPLES:-3}"
INTERVAL="${INTERVAL:-15}"

if [ "${1:-}" = "--init" ]; then
    USE_INIT="--init"
    echo "Control run: docker-init is PID 1, supavisor runs below it."
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
cleanup() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    rm -rf "$WORK"
}
trap cleanup EXIT

case "$(uname -m)" in
    arm64 | aarch64) ARCH=arm64 ;;
    *) ARCH=amd64 ;;
esac

echo "Building supavisor for linux/$ARCH..."
(cd "$REPO_ROOT" && GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$WORK/supavisor" ./cmd/supavisor)

# The wrapper backgrounds a grandchild and exits, so the grandchild is orphaned
# onto PID 1. autorestart keeps it happening, so a leak accumulates visibly
# rather than showing up as a single process that is easy to miss.
#
# The grandchild is 'tail' and the direct child is 'sleep' on purpose. Supavisor
# is PID 1 here, so its own direct child also has ppid=1 and the parentage alone
# cannot tell a running program from a leaked orphan. Different commands can:
# every (tail) that outlives its run is a leak.
cat > "$WORK/supavisor.yml" <<'EOF'
supavisor:
  pidfile: /tmp/supavisor.pid
  socket: /tmp/supavisor.sock
  log_level: error

programs:
  spawner:
    command: /bin/sh -c "tail -f /dev/null & exec sleep 2"
    autostart: true
    autorestart: always
    max_restarts: 50
    startsecs: 1
EOF

# Counts processes in state Z, which are exited children nobody has waited for.
count_zombies() {
    docker exec "$CONTAINER" sh -c '
        n=0
        for p in /proc/[0-9]*; do
            [ -r "$p/stat" ] || continue
            set -- $(cat "$p/stat" 2>/dev/null)
            [ "$3" = "Z" ] && n=$((n + 1))
        done
        echo $n'
}

process_table() {
    docker exec "$CONTAINER" sh -c '
        for p in /proc/[0-9]*; do
            [ -r "$p/stat" ] || continue
            set -- $(cat "$p/stat" 2>/dev/null)
            echo "  pid=$1 state=$3 ppid=$4 cmd=$2"
        done'
}

echo "Starting the container..."
# shellcheck disable=SC2086 # USE_INIT is a single optional flag, not a list
docker run -d $USE_INIT --name "$CONTAINER" -v "$WORK:/sv:ro" "$IMAGE" \
    /sv/supavisor -c /sv/supavisor.yml >/dev/null

echo "PID 1 is: $(docker exec "$CONTAINER" cat /proc/1/comm)"
echo

# Grandchildren that outlived their run. At most one is alive at any moment, so
# anything above that is accumulating, whether it is a zombie or still running.
count_orphans() {
    docker exec "$CONTAINER" sh -c 'ls /proc/[0-9]*/stat 2>/dev/null | while read -r s; do
        grep -c "(tail)" "$s" 2>/dev/null || true
    done | grep -c 1 || true'
}

for i in $(seq 1 "$SAMPLES"); do
    sleep "$INTERVAL"
    echo "t=$((i * INTERVAL))s  zombies: $(count_zombies)  grandchildren: $(count_orphans)"
done

echo
echo "Process table. A (tail) is a grandchild: at most one, and only while its"
echo "run is in flight. Several, or any in state Z, is the leak."
process_table
