#!/usr/bin/env bash

##
#  GenixSearch
#
#  Fast, lightweight and schema-less search backend
#  License: Mozilla Public License v2.0 (MPL v2.0)
##

set -euo pipefail

ABSPATH=$(cd "$(dirname "$0")"; pwd)
BASE_DIR="$ABSPATH/../"

CROSS_VERSION="${CROSS_VERSION:-0.2.5}"
CONTAINER_ENGINE="${CONTAINER_ENGINE:-podman}"
TARGET="${TARGET:-aarch64-unknown-linux-gnu}"
TARGET_CPU="${TARGET_CPU:-neoverse-n1}"
PROFILE="${PROFILE:-release}"

if ! command -v cargo >/dev/null 2>&1; then
  echo "Error: cargo is required but was not found in PATH" >&2
  exit 1
fi

if ! command -v "$CONTAINER_ENGINE" >/dev/null 2>&1; then
  echo "Error: $CONTAINER_ENGINE is required but was not found in PATH" >&2
  exit 1
fi

if [ "$PROFILE" != "release" ] && [ "$PROFILE" != "debug" ]; then
  echo "Error: PROFILE must be either 'release' or 'debug'" >&2
  exit 1
fi

if ! command -v cross >/dev/null 2>&1; then
  echo "cross was not found; installing cross v$CROSS_VERSION with cargo..."
  cargo install cross --version "$CROSS_VERSION" --locked
fi

build_args=()

if [ "$PROFILE" = "release" ]; then
  build_args+=(--release)
fi

echo "Building GenixSearch for Ampere / Arm Neoverse N1"
echo "  tool:       cross $(cross --version)"
echo "  engine:     $CONTAINER_ENGINE"
echo "  target:     $TARGET"
echo "  target-cpu: $TARGET_CPU"
echo "  profile:    $PROFILE"
echo

pushd "$BASE_DIR" >/dev/null
  CROSS_CONTAINER_ENGINE="$CONTAINER_ENGINE" \
  RUSTFLAGS="-C target-cpu=$TARGET_CPU" \
  cross build --locked --target "$TARGET" "${build_args[@]}"
popd >/dev/null

binary_path="$BASE_DIR/target/$TARGET/$PROFILE/genixsearch"

if [ -f "$binary_path" ]; then
  echo
  echo "Build complete: $binary_path"
else
  echo "Error: build finished but binary was not found at: $binary_path" >&2
  exit 1
fi
