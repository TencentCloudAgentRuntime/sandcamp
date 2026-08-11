#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

target=${RUST_TARGET:-x86_64-unknown-linux-musl}
build_output=$(
  RUSTFLAGS='-C linker=rust-lld' \
    cargo test -p campd --test startup --target "$target" --no-run 2>&1
)
printf '%s\n' "$build_output"

test_binary=$(
  printf '%s\n' "$build_output" |
    sed -n 's/^[[:space:]]*Executable tests\/startup.rs (\(.*\))$/\1/p' |
    tail -n 1
)
if [[ -z "$test_binary" || ! -x "$test_binary" ]]; then
  printf 'unable to resolve the campd Linux integration test binary\n' >&2
  exit 1
fi

repository=$(pwd -P)
case "$test_binary" in
  /*) test_binary_path=$test_binary ;;
  *) test_binary_path="$repository/$test_binary" ;;
esac

docker run --rm --platform linux/amd64 \
  -v "$repository:$repository:ro" \
  -w "$repository" \
  docker.io/library/alpine@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e \
  "$test_binary_path" --test-threads=1
