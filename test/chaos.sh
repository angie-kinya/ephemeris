#!/usr/bin/env bash
# chaos: SIGKILL the CLI mid-session; the sandbox must not outlive its lease
set -u
E=${1:?path to ephemeris binary}
export DOCKER_HOST=${DOCKER_HOST:-unix://$XDG_RUNTIME_DIR/podman/podman.sock}
count() { podman ps -a --filter label=dev.ephemeris.managed=true -q | wc -l; }

$E -f <(printf 'name: chaos\n') run -- sleep 600 &
pid=$!
for _ in $(seq 50); do [ "$(count)" -ge 1 ] && break; sleep 0.2; done
kill -9 "$pid"
echo "after kill -9: $(count) sandbox(es) left (expected 1)"
sleep 3; $E reap; echo "reap right away: $(count) left (expected 1, lease still fresh)"
sleep 40; $E reap; echo "reap after lease TTL: $(count) left (expected 0)"
[ "$(count)" -eq 0 ]