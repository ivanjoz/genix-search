##
#  GenixSearch
#
#  The single definition of what a release ships.
#
#  Sourced by `release_binaries.sh`, which builds these, and by
#  `publish_release.sh`, which uploads them. Keeping one list means the
#  publisher cannot look for an asset the builder never produces.
#
#  License: Mozilla Public License v2.0 (MPL v2.0)
##

# Every published build, in the order they should appear in a release.
RELEASE_BUILDS=(linux-amd64 linux-arm64 linux-arm64-ampere)

# Resolves a build name into `<target> <target-cpu> <asset-suffix>`, where a
# `target-cpu` of `-` means the target's default baseline.
#
# `neoverse-n1` is the core of the Ampere Altra / Altra Max, which is what
# Oracle Cloud rents out as the "Ampere A1" (VM.Standard.A1.Flex) shape.
release_build_spec() {
  case "$1" in
    linux-amd64)
      echo "x86_64-unknown-linux-musl - x86_64-linux-musl"
      ;;
    linux-arm64)
      echo "aarch64-unknown-linux-musl - aarch64-linux-musl"
      ;;
    linux-arm64-ampere)
      echo "aarch64-unknown-linux-musl neoverse-n1 aarch64-linux-musl-neoverse-n1"
      ;;
    *)
      echo "unknown build '$1' (expected one of: ${RELEASE_BUILDS[*]})" >&2
      return 1
      ;;
  esac
}

# The architecture `file` reports for a target's binaries, used to catch a
# mislabelled or stale artefact before it reaches a release.
release_expected_arch() {
  case "$1" in
    aarch64-*) echo "ARM aarch64" ;;
    x86_64-*)  echo "x86-64" ;;
    *)
      echo "no known architecture string for target '$1'" >&2
      return 1
      ;;
  esac
}
