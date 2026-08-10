#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

: "${SANDRUN_BIN:?SANDRUN_BIN is required}"
ALPINE_IMAGE=${ALPINE_IMAGE:-docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e}
DEBIAN_IMAGE=${DEBIAN_IMAGE:-docker.io/library/debian@sha256:362e64223cc0da95422b3b13c045186fc0a81250e765d31c025fbddf257f6143}
PYTHON_IMAGE=${PYTHON_IMAGE:-docker.io/library/python:3.11-slim-bookworm@sha256:77923445c077d8eb971b14b2b114a1d9cd4a87edb4c75654820ca4832ee8cb15}
RUNTIME_IMAGE=${RUNTIME_IMAGE:-sandcamp-runtime:poc-20260806}
FASTAPI_IMAGE=${FASTAPI_IMAGE:-sandcamp-fastapi-proxy:poc-20260806}
EGRESS_IMAGE=${EGRESS_IMAGE:-sandcamp-egress:poc-20260806}

SANDRUN_BIN=$(realpath "$SANDRUN_BIN")
test -x "$SANDRUN_BIN"

run_suffix="$(date -u +%Y%m%dT%H%M%SZ)-$$"
network="sandrun-net-${run_suffix}"
upstream="sandrun-upstream-${run_suffix}"
fastapi="sandrun-fastapi-${run_suffix}"
egress="sandrun-egress-${run_suffix}"
bind_runner="sandrun-bind-${run_suffix}"
signal_runner="sandrun-signal-${run_suffix}"
overlay_holder="sandrun-overlay-holder-${run_suffix}"
fastapi_volume="sandrun-fastapi-root-${run_suffix}"
egress_volume="sandrun-egress-root-${run_suffix}"
alpine_volume="sandrun-alpine-root-${run_suffix}"
debian_volume="sandrun-debian-root-${run_suffix}"
runtime_volume="sandrun-runtime-root-${run_suffix}"
state_volume="sandrun-state-${run_suffix}"
overlay_volume="sandrun-overlay-${run_suffix}"
export_container=

cleanup() {
  for exact in "$fastapi" "$egress" "$upstream" "$bind_runner" "$signal_runner" "$overlay_holder"; do
    if docker inspect "$exact" >/dev/null 2>&1; then
      docker rm -f "$exact" >/dev/null
    fi
  done
  if docker network inspect "$network" >/dev/null 2>&1; then
    docker network rm "$network" >/dev/null
  fi
  if [[ -n "$export_container" ]] && docker inspect "$export_container" >/dev/null 2>&1; then
    docker rm -f "$export_container" >/dev/null
  fi
  for volume in "$fastapi_volume" "$egress_volume" "$alpine_volume" "$debian_volume" "$runtime_volume" "$state_volume" "$overlay_volume"; do
    if docker volume inspect "$volume" >/dev/null 2>&1; then
      docker volume rm -f "$volume" >/dev/null
    fi
  done
}
trap cleanup EXIT

export_rootfs() {
  local image=$1
  local volume=$2
  docker volume create "$volume" >/dev/null
  export_container=$(docker create --platform linux/amd64 "$image")
  docker export "$export_container" |
    docker run --rm -i --platform linux/amd64 \
      -v "$volume:/rootfs" "$ALPINE_IMAGE" \
      tar --no-same-owner -C /rootfs -xf -
  docker rm "$export_container" >/dev/null
  export_container=
}

wait_http() {
  local url=$1
  local deadline=$((SECONDS + 30))
  while ((SECONDS < deadline)); do
    if curl --connect-timeout 1 --max-time 1 -fsS "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

export_rootfs "$FASTAPI_IMAGE" "$fastapi_volume"
export_rootfs "$EGRESS_IMAGE" "$egress_volume"
export_rootfs "$ALPINE_IMAGE" "$alpine_volume"
export_rootfs "$DEBIAN_IMAGE" "$debian_volume"
export_rootfs "$RUNTIME_IMAGE" "$runtime_volume"
for volume in "$fastapi_volume" "$egress_volume"; do
  docker run --rm --platform linux/amd64 -v "$volume:/rootfs" "$ALPINE_IMAGE" \
    rm -f /rootfs/etc/hosts /rootfs/etc/resolv.conf
done

docker volume create "$state_volume" >/dev/null
docker volume create "$overlay_volume" >/dev/null
docker run --rm --platform linux/amd64 -v "$state_volume:/state" "$ALPINE_IMAGE" \
  sh -c "mkdir -p /state/source-dir /state/user-output \
    && chown 65532:65532 /state/user-output \
    && : > /state/source-file"

expect_bind_failure() {
  local source=$1
  local target=$2
  local expected=$3
  local output
  local status
  set +e
  output=$(
    docker run --rm --platform linux/amd64 --privileged \
      --security-opt seccomp=unconfined \
      -v "$SANDRUN_BIN:/sandrun:ro" \
      -v "$fastapi_volume:/rootfs:ro" \
      -v "$state_volume:/state" \
      -v "$overlay_volume:/var/lib/sandcamp/overlay" \
      "$DEBIAN_IMAGE" \
      /sandrun --rootfs /rootfs --bind "$source" "$target" -- /usr/bin/true 2>&1
  )
  status=$?
  set -e
  test "$status" = "125"
  printf '%s\n' "$output" | rg "$expected" >/dev/null
}

expect_bind_failure /state/source-file /mnt/share incompatible_bind
expect_bind_failure /state/source-dir /etc/sandcamp-validation/probe.txt incompatible_bind
expect_bind_failure /state/source-file /missing-target mount_target_resolve_failed
expect_bind_failure /state/source-file /sandcamp-escape invalid_mount_target
printf 'sandrun_bind_target_validation=passed\n'

expect_workdir_failure() {
  local workdir=$1
  local expected=$2
  local output
  local status
  set +e
  output=$(
    docker run --rm --platform linux/amd64 --privileged \
      --security-opt seccomp=unconfined \
      -v "$SANDRUN_BIN:/sandrun:ro" \
      -v "$fastapi_volume:/rootfs:ro" \
      -v "$overlay_volume:/var/lib/sandcamp/overlay" \
      "$DEBIAN_IMAGE" \
      /sandrun --rootfs /rootfs --workdir "$workdir" -- /usr/bin/true 2>&1
  )
  status=$?
  set -e
  test "$status" = "125"
  printf '%s\n' "$output" | rg "$expected" >/dev/null
}

expect_workdir_failure /etc/passwd "workdir is not a directory"
expect_workdir_failure /missing-workdir mount_target_resolve_failed
printf 'sandrun_workdir_validation=passed\n'

docker run --name "$bind_runner" --platform linux/amd64 --privileged \
  --security-opt seccomp=unconfined \
  -v "$SANDRUN_BIN:/sandrun:ro" \
  -v "$fastapi_volume:/rootfs:ro" \
  -v "$state_volume:/state" \
  -v "$overlay_volume:/var/lib/sandcamp/overlay" \
  "$DEBIAN_IMAGE" \
  /sandrun --rootfs /rootfs --workdir /mnt --standard-mounts \
  --bind /state /mnt -- \
  /bin/sh -c 'printf "%s|bound" "$(pwd -P)" > marker' >/dev/null
docker rm "$bind_runner" >/dev/null
test "$(docker run --rm --platform linux/amd64 -v "$state_volume:/state:ro" "$ALPINE_IMAGE" cat /state/marker)" = "/mnt|bound"
printf 'sandrun_bind_mount=passed\n'
printf 'sandrun_post_pivot_workdir=passed\n'

docker run --rm --platform linux/amd64 --privileged \
  --security-opt seccomp=unconfined \
  -v "$SANDRUN_BIN:/sandrun:ro" \
  -v "$fastapi_volume:/rootfs:ro" \
  -v "$state_volume:/state" \
  -v "$overlay_volume:/var/lib/sandcamp/overlay" \
  "$DEBIAN_IMAGE" \
  /sandrun --rootfs /rootfs --overlay-id named-user \
  --workdir /home/app --standard-mounts --user app \
  --bind /state/user-output /mnt/share -- \
  /bin/sh -c '{
    printf "uid=%s\n" "$(id -u)"
    printf "gid=%s\n" "$(id -g)"
    printf "cwd=%s\n" "$(pwd -P)"
    grep -E "^(Uid|Gid|Groups|CapInh|CapEff|CapPrm|CapAmb|NoNewPrivs):" /proc/self/status
  } > /mnt/share/identity; printf writable > /home/app/overlay-write'
identity=$(
  docker run --rm --platform linux/amd64 -v "$state_volume:/state:ro" \
    "$ALPINE_IMAGE" cat /state/user-output/identity
)
printf '%s\n' "$identity" | rg '^uid=65532$' >/dev/null
printf '%s\n' "$identity" | rg '^gid=65532$' >/dev/null
printf '%s\n' "$identity" | rg '^cwd=/home/app$' >/dev/null
printf '%s\n' "$identity" | rg '^Uid:[[:space:]]+65532[[:space:]]+65532[[:space:]]+65532[[:space:]]+65532$' >/dev/null
printf '%s\n' "$identity" | rg '^Gid:[[:space:]]+65532[[:space:]]+65532[[:space:]]+65532[[:space:]]+65532$' >/dev/null
printf '%s\n' "$identity" | rg '^Groups:[[:space:]]*$' >/dev/null
printf '%s\n' "$identity" | rg '^CapInh:[[:space:]]+0+$' >/dev/null
printf '%s\n' "$identity" | rg '^CapEff:[[:space:]]+0+$' >/dev/null
printf '%s\n' "$identity" | rg '^CapPrm:[[:space:]]+0+$' >/dev/null
printf '%s\n' "$identity" | rg '^CapAmb:[[:space:]]+0+$' >/dev/null
printf '%s\n' "$identity" | rg '^NoNewPrivs:[[:space:]]+1$' >/dev/null
docker run --rm --platform linux/amd64 -v "$fastapi_volume:/rootfs:ro" \
  "$ALPINE_IMAGE" test ! -e /rootfs/home/app/overlay-write
printf 'sandrun_named_user=passed\n'
printf 'sandrun_named_user_overlay_write=passed\n'

set +e
missing_user_output=$(
  docker run --rm --platform linux/amd64 --privileged \
    --security-opt seccomp=unconfined \
    -v "$SANDRUN_BIN:/sandrun:ro" \
    -v "$fastapi_volume:/rootfs:ro" \
    -v "$overlay_volume:/var/lib/sandcamp/overlay" \
    "$DEBIAN_IMAGE" \
    /sandrun --rootfs /rootfs --user missing-user -- /usr/bin/true 2>&1
)
missing_user_status=$?
set -e
test "$missing_user_status" = "125"
printf '%s\n' "$missing_user_output" | rg 'user_not_found: missing-user$' >/dev/null
printf 'sandrun_missing_user_failure=passed\n'

for loader_case in alpine debian; do
  if [[ "$loader_case" == "alpine" ]]; then
    loader_volume=$alpine_volume
  else
    loader_volume=$debian_volume
  fi
  docker run --rm --platform linux/amd64 --privileged \
    --security-opt seccomp=unconfined \
    -v "$SANDRUN_BIN:/sandrun:ro" \
    -v "$loader_volume:/rootfs:ro" \
    -v "$state_volume:/state" \
    -v "$overlay_volume:/var/lib/sandcamp/overlay" \
    "$DEBIAN_IMAGE" \
    /sandrun --rootfs /rootfs --bind /state /tmp -- \
    /bin/sh -c "printf '%s' '$loader_case' > /tmp/loader-$loader_case"
  test "$(
    docker run --rm --platform linux/amd64 -v "$state_volume:/state:ro" \
      "$ALPINE_IMAGE" cat "/state/loader-$loader_case"
  )" = "$loader_case"
done
static_output=$(
  docker run --rm --platform linux/amd64 --privileged \
    --security-opt seccomp=unconfined \
    -v "$SANDRUN_BIN:/sandrun:ro" \
    -v "$runtime_volume:/rootfs:ro" \
    -v "$overlay_volume:/var/lib/sandcamp/overlay" \
    "$DEBIAN_IMAGE" \
    /sandrun --rootfs /rootfs -- /bin/sandrun --version
)
test "$static_output" = "sandrun 0.0.0"
printf 'sandrun_musl_loader=passed\n'
printf 'sandrun_glibc_loader=passed\n'
printf 'sandrun_static_binary=passed\n'

docker run -d --name "$overlay_holder" --platform linux/amd64 --privileged \
  --security-opt seccomp=unconfined \
  -v "$SANDRUN_BIN:/sandrun:ro" \
  -v "$alpine_volume:/rootfs:ro" \
  -v "$state_volume:/state" \
  -v "$overlay_volume:/var/lib/sandcamp/overlay" \
  "$DEBIAN_IMAGE" \
  /sandrun --rootfs /rootfs --overlay-id stable-layer --bind /state /mnt -- \
  /bin/sh -c 'printf persisted > /root/sandcamp-persisted; : > /mnt/overlay-lock-ready; while :; do sleep 1; done' >/dev/null
overlay_deadline=$((SECONDS + 10))
while ((SECONDS < overlay_deadline)); do
  if docker run --rm --platform linux/amd64 -v "$state_volume:/state:ro" \
    "$ALPINE_IMAGE" test -f /state/overlay-lock-ready; then
    break
  fi
  sleep 0.1
done
docker run --rm --platform linux/amd64 -v "$state_volume:/state:ro" \
  "$ALPINE_IMAGE" test -f /state/overlay-lock-ready
set +e
overlay_busy_output=$(
  docker run --rm --platform linux/amd64 --privileged \
    --security-opt seccomp=unconfined \
    -v "$SANDRUN_BIN:/sandrun:ro" \
    -v "$alpine_volume:/rootfs:ro" \
    -v "$overlay_volume:/var/lib/sandcamp/overlay" \
    "$DEBIAN_IMAGE" \
    /sandrun --rootfs /rootfs --overlay-id stable-layer -- /bin/true 2>&1
)
overlay_busy_status=$?
set -e
test "$overlay_busy_status" = "125"
printf '%s\n' "$overlay_busy_output" | rg 'overlay_workspace_busy' >/dev/null
docker stop --time 2 "$overlay_holder" >/dev/null
docker rm "$overlay_holder" >/dev/null
docker run --rm --platform linux/amd64 --privileged \
  --security-opt seccomp=unconfined \
  -v "$SANDRUN_BIN:/sandrun:ro" \
  -v "$alpine_volume:/rootfs:ro" \
  -v "$state_volume:/state" \
  -v "$overlay_volume:/var/lib/sandcamp/overlay" \
  "$DEBIAN_IMAGE" \
  /sandrun --rootfs /rootfs --overlay-id stable-layer --bind /state /mnt -- \
  /bin/sh -c 'cat /root/sandcamp-persisted > /mnt/overlay-persisted'
test "$(
  docker run --rm --platform linux/amd64 -v "$state_volume:/state:ro" \
    "$ALPINE_IMAGE" cat /state/overlay-persisted
)" = "persisted"
printf 'sandrun_overlay_identity_lock=passed\n'
printf 'sandrun_overlay_restart_persistence=passed\n'

docker run -d --name "$signal_runner" --platform linux/amd64 --privileged \
  --security-opt seccomp=unconfined \
  -v "$SANDRUN_BIN:/sandrun:ro" \
  -v "$alpine_volume:/rootfs:ro" \
  -v "$state_volume:/state" \
  -v "$overlay_volume:/var/lib/sandcamp/overlay" \
  "$DEBIAN_IMAGE" \
  /sandrun --rootfs /rootfs --bind /state /tmp -- \
  /bin/sh -c 'trap "printf TERM > /tmp/signal-received; exit 0" TERM; : > /tmp/signal-ready; while :; do sleep 1; done' >/dev/null
signal_deadline=$((SECONDS + 10))
while ((SECONDS < signal_deadline)); do
  if docker run --rm --platform linux/amd64 -v "$state_volume:/state:ro" \
    "$ALPINE_IMAGE" test -f /state/signal-ready; then
    break
  fi
  sleep 0.1
done
docker run --rm --platform linux/amd64 -v "$state_volume:/state:ro" \
  "$ALPINE_IMAGE" test -f /state/signal-ready
docker stop --time 5 "$signal_runner" >/dev/null
test "$(
  docker run --rm --platform linux/amd64 -v "$state_volume:/state:ro" \
    "$ALPINE_IMAGE" cat /state/signal-received
)" = "TERM"
docker rm "$signal_runner" >/dev/null
printf 'sandrun_exec_signal=passed\n'

docker network create "$network" >/dev/null
docker run -d --platform linux/amd64 --name "$upstream" --network "$network" \
  "$PYTHON_IMAGE" python -m http.server 8080 >/dev/null
docker run -d --platform linux/amd64 --privileged --security-opt seccomp=unconfined \
  --name "$fastapi" --network "$network" -p 127.0.0.1::9200 \
  -e UPSTREAM_URL="http://${upstream}:8080/" -e FASTAPI_PORT=9200 \
  -v "$SANDRUN_BIN:/sandrun:ro" \
  -v "$fastapi_volume:/rootfs:ro" \
  -v "$overlay_volume:/var/lib/sandcamp/overlay" \
  "$DEBIAN_IMAGE" \
  /sandrun --rootfs /rootfs --standard-mounts -- \
  /usr/local/bin/python /opt/fastapi-proxy/app.py >/dev/null
fastapi_port=$(docker port "$fastapi" 9200/tcp)
fastapi_port=${fastapi_port##*:}
if ! wait_http "http://127.0.0.1:${fastapi_port}/healthz"; then
  docker logs "$fastapi"
  exit 1
fi
test "$(curl --connect-timeout 1 --max-time 2 -fsS "http://127.0.0.1:${fastapi_port}/healthz")" = '{"status":"ok"}'
curl --connect-timeout 1 --max-time 2 -fsS "http://127.0.0.1:${fastapi_port}/" >/dev/null
overlay_evidence=$(curl --connect-timeout 1 --max-time 2 -fsS "http://127.0.0.1:${fastapi_port}/sandcamp/evidence")
OVERLAY_EVIDENCE="$overlay_evidence" python3 - <<'PY'
import json
import os

evidence = json.loads(os.environ["OVERLAY_EVIDENCE"])
assert evidence["overlay"]["root_filesystem"] == "overlay", evidence
assert all(item["written"] for item in evidence["overlay"]["writes"].values()), evidence
PY
docker run --rm --platform linux/amd64 -v "$fastapi_volume:/rootfs:ro" "$ALPINE_IMAGE" \
  sh -c "test ! -e /rootfs/var/lib/sandcamp-validation/cache.txt \
    && test ! -e /rootfs/root/.cache/sandcamp-validation.txt \
    && test ! -e /rootfs/etc/sandcamp-overlay.txt"
printf 'sandrun_fastapi_rootfs=passed port=%s\n' "$fastapi_port"
printf 'sandrun_overlay_lower_unchanged=passed\n'
docker rm -f "$fastapi" "$upstream" >/dev/null
docker network rm "$network" >/dev/null

docker run -d --platform linux/amd64 --privileged --security-opt seccomp=unconfined \
  --name "$egress" -p 127.0.0.1::24774 \
  -e OPENSANDBOX_EGRESS_MODE=dns \
  -e OPENSANDBOX_EGRESS_HTTP_ADDR=:24774 \
  -e OPENSANDBOX_EGRESS_RULES='{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com"},{"action":"allow","target":"*.example.com"}]}' \
  -v "$SANDRUN_BIN:/sandrun:ro" \
  -v "$egress_volume:/rootfs:ro" \
  -v "$overlay_volume:/var/lib/sandcamp/overlay" \
  "$DEBIAN_IMAGE" \
  /sandrun --rootfs /rootfs --standard-mounts -- \
  /opt/opensandbox-egress/egress >/dev/null
egress_port=$(docker port "$egress" 24774/tcp)
egress_port=${egress_port##*:}
if ! wait_http "http://127.0.0.1:${egress_port}/healthz"; then
  docker logs "$egress"
  exit 1
fi
test "$(curl --connect-timeout 1 --max-time 2 -fsS "http://127.0.0.1:${egress_port}/healthz")" = "ok"
docker logs "$egress" 2>&1 | rg 'DNS redirect installed successfully' >/dev/null
printf 'sandrun_egress_rootfs=passed port=%s\n' "$egress_port"

printf 'sandrun_linux_gate=passed\n'
