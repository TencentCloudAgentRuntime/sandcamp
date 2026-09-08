#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

: "${SANDRUN_BIN:?SANDRUN_BIN is required}"
DIND_IMAGE=${DIND_IMAGE:-docker.io/library/docker:28-dind@sha256:2a232a42256f70d78e3cc5d2b5d6b3276710a0de0596c145f627ecfae90282ac}
ALPINE_IMAGE=${ALPINE_IMAGE:-docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e}
DEBIAN_IMAGE=${DEBIAN_IMAGE:-docker.io/library/debian@sha256:362e64223cc0da95422b3b13c045186fc0a81250e765d31c025fbddf257f6143}

SANDRUN_BIN=$(realpath "$SANDRUN_BIN")
test -x "$SANDRUN_BIN"

suffix="$(date -u +%Y%m%dT%H%M%SZ)-$$"
root_volume="sandcamp-dind-root-${suffix}"
data_volume="sandcamp-dind-data-${suffix}"
overlay_volume="sandcamp-dind-overlay-${suffix}"
export_container=

cleanup() {
  if [[ -n "$export_container" ]] && docker inspect "$export_container" >/dev/null 2>&1; then
    docker rm -f "$export_container" >/dev/null
  fi
  for volume in "$root_volume" "$data_volume" "$overlay_volume"; do
    if docker volume inspect "$volume" >/dev/null 2>&1; then
      docker volume rm -f "$volume" >/dev/null
    fi
  done
}
trap cleanup EXIT

for volume in "$root_volume" "$data_volume" "$overlay_volume"; do
  docker volume create "$volume" >/dev/null
done

export_container=$(docker create --platform linux/amd64 "$DIND_IMAGE")
docker export "$export_container" |
  docker run --rm -i --platform linux/amd64 \
    -v "$root_volume:/rootfs" "$ALPINE_IMAGE" \
    tar --no-same-owner -C /rootfs -xf -
docker rm "$export_container" >/dev/null
export_container=

docker run --rm --platform linux/amd64 --privileged \
  --security-opt seccomp=unconfined \
  -v "$SANDRUN_BIN:/sandrun:ro" \
  -v "$root_volume:/rootfs:ro" \
  -v "$data_volume:/docker-data" \
  -v "$overlay_volume:/var/lib/sandcamp/overlay" \
  "$DEBIAN_IMAGE" \
  /sandrun --rootfs /rootfs --standard-mounts \
    --bind /docker-data /var/lib/docker -- \
  /bin/sh -ec '
    filesystem=$(stat -f -c %T /var/lib/docker)
    test "$filesystem" != overlayfs

    dockerd --host=unix:///run/docker.sock --data-root=/var/lib/docker \
      --storage-driver=overlay2 --iptables=false --bridge=none \
      --ip-forward=false --ip-masq=false >/tmp/dockerd.log 2>&1 &
    daemon=$!
    trap '\''kill "$daemon" 2>/dev/null || true; wait "$daemon" 2>/dev/null || true'\'' EXIT

    attempt=0
    until docker --host=unix:///run/docker.sock info >/dev/null 2>&1; do
      if ! kill -0 "$daemon" 2>/dev/null; then
        cat /tmp/dockerd.log
        exit 1
      fi
      attempt=$((attempt + 1))
      if test "$attempt" -ge 60; then
        cat /tmp/dockerd.log
        exit 1
      fi
      sleep 1
    done

    docker --host=unix:///run/docker.sock info \
      --format '\''driver={{.Driver}} root={{.DockerRootDir}} cgroup={{.CgroupVersion}}'\''
    docker --host=unix:///run/docker.sock run --rm --network=none \
      alpine:3.22 /bin/true
  '

printf 'sandrun_nested_docker=passed\n'
