#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

SCENARIO=${SCENARIO:-root}
if [[ "$SCENARIO" != "root" && "$SCENARIO" != "nonroot" ]]; then
  printf 'unsupported SCENARIO=%s\n' "$SCENARIO" >&2
  exit 2
fi

ALPINE_IMAGE=${ALPINE_IMAGE:-docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e}
PYTHON_IMAGE=${PYTHON_IMAGE:-docker.io/library/python:3.11-slim-bookworm@sha256:77923445c077d8eb971b14b2b114a1d9cd4a87edb4c75654820ca4832ee8cb15}
RUNTIME_IMAGE=${RUNTIME_IMAGE:-sandcamp-runtime:poc-20260806}
FASTAPI_IMAGE=${FASTAPI_IMAGE:-sandcamp-fastapi-proxy:poc-20260806}
EGRESS_IMAGE=${EGRESS_IMAGE:-sandcamp-egress:poc-20260806}

run_suffix="$(date -u +%Y%m%dT%H%M%SZ)-$$"
container="sandcamp-stack-${run_suffix}"
runtime_volume="sandcamp-runtime-root-${run_suffix}"
fastapi_volume="sandcamp-fastapi-root-${run_suffix}"
egress_volume="sandcamp-egress-root-${run_suffix}"
shared_volume="sandcamp-shared-${run_suffix}"
config_volume="sandcamp-config-${run_suffix}"
overlay_volume="sandcamp-overlay-${run_suffix}"
export_container=

cleanup() {
  if docker inspect "$container" >/dev/null 2>&1; then
    docker rm -f "$container" >/dev/null
  fi
  if [[ -n "$export_container" ]] && docker inspect "$export_container" >/dev/null 2>&1; then
    docker rm -f "$export_container" >/dev/null
  fi
  for volume in "$runtime_volume" "$fastapi_volume" "$egress_volume" "$shared_volume" "$config_volume" "$overlay_volume"; do
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

export_rootfs "$RUNTIME_IMAGE" "$runtime_volume"
export_rootfs "$FASTAPI_IMAGE" "$fastapi_volume"
export_rootfs "$EGRESS_IMAGE" "$egress_volume"
test "$(
  docker run --rm --platform linux/amd64 -v "$runtime_volume:/rootfs:ro" \
    "$ALPINE_IMAGE" stat -c '%u:%g:%a' /rootfs/bin/campd
)" = "0:0:4755"
test "$(
  docker run --rm --platform linux/amd64 -v "$runtime_volume:/rootfs:ro" \
    "$ALPINE_IMAGE" stat -c '%u:%g:%a' /rootfs/bin/campd.real
)" = "0:0:755"
test "$(
  docker run --rm --platform linux/amd64 -v "$runtime_volume:/rootfs:ro" \
    "$ALPINE_IMAGE" stat -c '%u:%g:%a' /rootfs/bin/sandrun
)" = "0:0:755"
docker volume create "$shared_volume" >/dev/null
docker volume create "$config_volume" >/dev/null
docker volume create "$overlay_volume" >/dev/null
docker run --rm --platform linux/amd64 -v "$shared_volume:/share" "$ALPINE_IMAGE" \
  chmod 0777 /share
docker run --rm --platform linux/amd64 -v "$config_volume:/config" "$ALPINE_IMAGE" \
  sh -c "printf '%s' 'readonly-from-main-image' > /config/probe.txt && chmod 0444 /config/probe.txt"
for volume in "$fastapi_volume" "$egress_volume"; do
  docker run --rm --platform linux/amd64 -v "$volume:/rootfs" "$ALPINE_IMAGE" \
    rm -f /rootfs/etc/hosts /rootfs/etc/resolv.conf
done

container_user=()
if [[ "$SCENARIO" == "nonroot" ]]; then
  spec=$(SANDCAMP_TEST_NONROOT=1 go run ./test/harness/render-spec)
  container_user=(--user 65532:65532)
else
  spec=$(go run ./test/harness/render-spec)
fi

docker run -d --name "$container" --platform linux/amd64 \
  --privileged --security-opt seccomp=unconfined \
  "${container_user[@]}" \
  -p 127.0.0.1::49982 -p 127.0.0.1::9200 \
  -e "SANDCAMP_SPEC=$spec" \
  -e "SANDCAMP_MAIN_IMAGE_ENV=from-main-image" \
  -v "$runtime_volume:/mnt/sandcamp:ro" \
  -v "$fastapi_volume:/mnt/fastapi:ro" \
  -v "$egress_volume:/mnt/egress:ro" \
  -v "$shared_volume:/sandcamp-validation/share" \
  -v "$config_volume:/sandcamp-validation/config:ro" \
  -v "$overlay_volume:/var/lib/sandcamp/overlay" \
  "$PYTHON_IMAGE" \
  /mnt/sandcamp/bin/campd -- >/dev/null

ready_port=$(docker port "$container" 49982/tcp)
ready_port=${ready_port##*:}
proxy_port=$(docker port "$container" 9200/tcp)
proxy_port=${proxy_port##*:}

if ! wait_http "http://127.0.0.1:${ready_port}/ready"; then
  docker logs "$container"
  exit 1
fi
set +e
launcher_reentry=$(docker exec "$container" /mnt/sandcamp/bin/campd -- 2>&1)
launcher_status=$?
set -e
test "$launcher_status" = "125"
printf '%s\n' "$launcher_reentry" | rg 'refusing non-PID-1 invocation' >/dev/null
test "$(curl --connect-timeout 1 --max-time 2 -fsS "http://127.0.0.1:${proxy_port}/healthz")" = '{"status":"ok"}'
curl --connect-timeout 1 --max-time 2 -fsS "http://127.0.0.1:${proxy_port}/" >/dev/null
if [[ "$SCENARIO" == "root" ]]; then
  test "$(curl --connect-timeout 1 --max-time 2 -fsS "http://127.0.0.1:${proxy_port}/docs")" = '{"status":"upstream"}'
fi

sidecar_evidence=$(curl --connect-timeout 1 --max-time 2 -fsS "http://127.0.0.1:${proxy_port}/sandcamp/evidence")
SIDECAR_EVIDENCE="$sidecar_evidence" python3 - <<'PY'
import json
import os

evidence = json.loads(os.environ["SIDECAR_EVIDENCE"])
assert evidence["process_env"] == "from-process-spec", evidence
assert evidence["main_image_env"] is None, evidence
assert evidence["image_config_env"] is None, evidence
assert evidence["overlay"]["root_filesystem"] == "overlay", evidence
assert all(item["written"] for item in evidence["overlay"]["writes"].values()), evidence
assert evidence["shared_write"]["written"] is True, evidence
assert evidence["shared_main_file"] == "created-by-main", evidence
assert evidence["readonly_config"] == "readonly-from-main-image", evidence
assert evidence["readonly_write_blocked"] is True, evidence
PY

docker run --rm --platform linux/amd64 -v "$fastapi_volume:/rootfs:ro" "$ALPINE_IMAGE" \
  sh -c "test ! -e /rootfs/var/lib/sandcamp-validation/cache.txt \
    && test ! -e /rootfs/root/.cache/sandcamp-validation.txt \
    && test ! -e /rootfs/etc/sandcamp-overlay.txt"

if [[ "$SCENARIO" == "root" ]]; then
  main_evidence=$(curl --connect-timeout 1 --max-time 2 -fsS "http://127.0.0.1:${proxy_port}/sandcamp/main-evidence")
  MAIN_EVIDENCE="$main_evidence" python3 - <<'PY'
import json
import os

evidence = json.loads(os.environ["MAIN_EVIDENCE"])
assert evidence["main_image_env"] == "from-main-image", evidence
assert evidence["main_process_env"] == "from-main-process-spec", evidence
assert evidence["main_write_error"] is None, evidence
assert evidence["main_file"] == "created-by-main", evidence
assert evidence["sidecar_file"] == "created-by-fastapi", evidence
assert evidence["readonly_config"] == "readonly-from-main-image", evidence
assert evidence["readonly_write_blocked"] is True, evidence
PY
  websocket_echo=$(
    docker exec "$container" \
      /mnt/sandcamp/bin/sandrun \
      --rootfs /mnt/fastapi \
      --workdir /opt/fastapi-proxy \
      --standard-mounts \
      -- \
      /usr/local/bin/python \
      /opt/fastapi-proxy/ws_check.py \
      http://127.0.0.1:9200/echo
  )
  test "$websocket_echo" = "sandcamp-websocket"

  netfilter_rules=$(
    docker exec "$container" \
      /mnt/sandcamp/bin/sandrun \
      --rootfs /mnt/egress \
      --standard-mounts \
      -- \
      /usr/sbin/iptables-save
  )
  printf '%s\n' "$netfilter_rules" | rg -- '--dport 53' >/dev/null
  printf '%s\n' "$netfilter_rules" | rg -- '-j (REDIRECT|DNAT)' >/dev/null

  egress_policy=$(
    docker exec "$container" /usr/local/bin/python -c \
      'import http.client; connection = http.client.HTTPConnection("127.0.0.1", 24774, timeout=2); connection.request("GET", "/policy"); response = connection.getresponse(); print(response.read().decode()); raise SystemExit(0 if response.status == 200 else 1)'
  )
  EGRESS_POLICY="$egress_policy" python3 - <<'PY'
import json
import os

response = json.loads(os.environ["EGRESS_POLICY"])
policy = response.get("policy", response)
assert policy["defaultAction"] == "deny", response
rules = {(rule["action"], rule["target"]) for rule in policy["egress"]}
assert ("allow", "example.com") in rules, response
assert ("allow", "*.example.com") in rules, response
PY
  docker exec "$container" /usr/local/bin/python -c \
    'import socket; socket.getaddrinfo("example.com", 443, type=socket.SOCK_STREAM)'
  if docker exec "$container" /usr/local/bin/python -c \
    'import socket; socket.getaddrinfo("example.org", 443, type=socket.SOCK_STREAM)' >/dev/null 2>&1; then
    printf '%s\n' 'Egress deny rule unexpectedly resolved example.org' >&2
    exit 1
  fi
else
  nonroot_identity=$(
    docker run --rm --platform linux/amd64 -v "$shared_volume:/share:ro" \
      "$ALPINE_IMAGE" cat /share/nonroot-identity.json
  )
  NONROOT_IDENTITY="$nonroot_identity" python3 - <<'PY'
import json
import os

evidence = json.loads(os.environ["NONROOT_IDENTITY"])
assert evidence["uid"] == 65532, evidence
assert evidence["gid"] == 65532, evidence
assert evidence["groups"] == [], evidence
assert evidence["status"]["CapEff"] == "0000000000000000", evidence
assert evidence["status"]["NoNewPrivs"] == "1", evidence
PY
  test "$(
    docker run --rm --platform linux/amd64 -v "$shared_volume:/share:ro" \
      "$ALPINE_IMAGE" stat -c '%u:%g:%a' /share/main-created.txt
  )" = "65532:65532:644"
  test "$(
    docker run --rm --platform linux/amd64 -v "$shared_volume:/share:ro" \
      "$ALPINE_IMAGE" stat -c '%u:%g:%a' /share/sidecar-created.txt
  )" = "0:0:644"
fi

docker stop --time 7 "$container" >/dev/null
test "$(docker inspect "$container" --format '{{.State.ExitCode}}')" = "143"

printf 'campd_ready=passed port=%s\n' "$ready_port"
printf 'campd_setuid_layout=passed\n'
printf 'campd_launcher_pid1_guard=passed\n'
printf 'fastapi_proxy=passed port=%s\n' "$proxy_port"
if [[ "$SCENARIO" == "root" ]]; then
  printf 'fastapi_websocket=passed\n'
  printf 'main_image_environment=passed\n'
else
  printf 'campd_nonroot_user_drop=passed\n'
  printf 'campd_nonroot_no_new_privs=passed\n'
  printf 'shared_cross_user_modes=passed\n'
fi
printf 'sidecar_process_environment=passed\n'
printf 'sidecar_environment_isolation=passed\n'
printf 'image_volume_oci_environment_not_applied=passed\n'
printf 'overlay_rootfs_writes=passed\n'
printf 'overlay_lower_unchanged=passed\n'
printf 'shared_readwrite_bind=passed\n'
printf 'shared_readonly_bind=passed\n'
if [[ "$SCENARIO" == "root" ]]; then
  printf 'egress_shared_network=passed\n'
  printf 'egress_policy_loaded=passed\n'
  printf 'egress_allow_rule=passed domain=example.com\n'
  printf 'egress_deny_rule=passed domain=example.org\n'
fi
printf 'campd_process_group_shutdown=passed\n'
printf 'sandcamp_stack_gate=passed scenario=%s\n' "$SCENARIO"
