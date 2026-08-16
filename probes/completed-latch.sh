#!/usr/bin/env bash
#
# Does a completed one-off keep its dependents startable after it has exited?
#
# A one-off is RUNNING only while its work is in flight, so the interesting
# moments are both after that window closes: a dependent must start once the work
# is done, and must still be able to start much later, when the task has been
# sitting in EXITED for a while. That second case is the one that used to be
# unrecoverable, and it is what condition: completed latches for.
#
#   ./probes/completed-latch.sh             condition: completed, the case under test
#   ./probes/completed-latch.sh --started   control: condition: started instead
#
# See probes/README.md for what a correct run looks like.
set -euo pipefail

CONDITION="completed"
CONTAINER="supavisor-completed-latch"
IMAGE="${IMAGE:-alpine:3}"

# How long the task works for, and how long the dependent is left up afterwards
# before it is taken down. The wait is what makes this a probe rather than a unit
# test: the failure being reproduced is one that only appears once the run is
# well and truly over.
WORK_SECS="${WORK_SECS:-5}"
SETTLE_SECS="${SETTLE_SECS:-20}"

if [ "${1:-}" = "--started" ]; then
    CONDITION="started"
    echo "Control run: the dependent waits for condition: started."
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

echo "Building supavisor and sctl for linux/$ARCH..."
(cd "$REPO_ROOT" && GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$WORK/supavisor" ./cmd/supavisor)
(cd "$REPO_ROOT" && GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$WORK/sctl" ./cmd/sctl)

# migrate takes long enough to be observably RUNNING, which is what makes the
# control fail visibly: condition: started lets the dependent through during that
# window rather than after it.
cat > "$WORK/supavisor.yml" <<EOF
supavisor:
  pidfile: /tmp/supavisor.pid
  socket: /tmp/supavisor.sock
  log_level: error

programs:
  migrate:
    command: /bin/sh -c "sleep $WORK_SECS; exit 0"
    autostart: true
    autorestart: never
    startsecs: 1

  api:
    command: /bin/sleep 3600
    autostart: true
    autorestart: never
    startsecs: 1
    depends_on:
      migrate:
        condition: $CONDITION
EOF

sctl() {
    docker exec "$CONTAINER" /sv/sctl -s /tmp/supavisor.sock "$@"
}

state() {
    sctl status "$1" 2>/dev/null | awk '$1 == "State:" { print $2 }'
}

echo "Starting the container..."
docker run -d --name "$CONTAINER" -v "$WORK:/sv:ro" "$IMAGE" \
    /sv/supavisor -c /sv/supavisor.yml >/dev/null

# Give the daemon its socket before anything asks it questions.
for _ in $(seq 1 20); do
    sctl status >/dev/null 2>&1 && break
    sleep 0.5
done

echo
echo "Phase 1: the dependent must start after the work finishes, not alongside it."
for i in $(seq 1 $((WORK_SECS + 3))); do
    sleep 1
    printf 't=%-3s migrate=%-8s api=%s\n' "${i}s" "$(state migrate)" "$(state api)"
done

echo
echo "Phase 2: the dependent goes down long after the task completed."
echo "Leaving it up for ${SETTLE_SECS}s first, so the run is well over."
sleep "$SETTLE_SECS"

echo -n "  sctl stop api ... "
if sctl stop api >/dev/null 2>&1; then echo "ok"; else echo "FAILED"; fi

echo -n "  sctl start api ... "
started=$(date +%s)
if sctl start api >/dev/null 2>&1; then result="ok"; else result="REFUSED"; fi
echo "$result ($(($(date +%s) - started))s)"

echo
# A daemon that died during the run is one of the outcomes worth seeing, so
# neither of these may take the script down with set -e before it reports.
sctl status || true

# Only a program with something to explain has a reason, so this is the control
# run's line: it names what the dependent is still waiting for.
reason="$(sctl status api | sed -n 's/^Reason: *//p' || true)"
if [ -n "$reason" ]; then
    echo
    echo "Why api is not running: $reason"
fi
