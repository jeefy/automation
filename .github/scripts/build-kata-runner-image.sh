#!/usr/bin/env bash
# Convert the Packer-built GHA runner VM image (image.raw) into a container
# image for kata-containers runners, so oracle-kata-* jobs run the software of
# the oracle-* VM runners (same actions/runner-images build, same docker).
#
# The rootfs is tens of GB and GHCR rejects layers over 10 GiB, so the tree is
# planned and packed by kata_rootfs_image.py into bounded tar layers. The source
# disk is mounted read-only and never modified; ARC adaptations (runner user,
# /home/runner, sanitised accounts, provenance file) live in a final layer.
#
# Usage:
#   build-kata-runner-image.sh <image.raw|rootfs-dir> <image-ref> <arch> <runner-images-tag>
# Example:
#   build-kata-runner-image.sh ci/gha-runner-vm/build/image.raw \
#     ghcr.io/cncf/gha-kata-runner:rc-20260901.1-amd64 amd64 ubuntu24/20260901.1
#
# Environment:
#   LAYER_LIMIT_BYTES   max uncompressed bytes per layer (default 9 GiB)
#   WORK_DIR            scratch directory (default: mktemp under /tmp)
#   KEEP_WORK=1         keep WORK_DIR for inspection
#   CRANE               crane binary (default: crane from PATH)
#   SOURCE_REVISION     git SHA recorded in labels and /etc/cncf-kata-runner-release
#   SOURCE_DATE_EPOCH   mtime/created timestamp for generated files (default 0 = deterministic)
#   KATA_IMAGE_OUTPUT   file receiving "digest=..." and "ref=..." lines
#   GITHUB_OUTPUT       if set, the same keys are appended for the workflow step
#
# A directory instead of image.raw skips the loop mount (used by the tests).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLANNER="${SCRIPT_DIR}/kata_rootfs_image.py"

usage() {
  echo "usage: $0 <image.raw|rootfs-dir> <image-ref> <arch> <runner-images-tag>" >&2
  exit 2
}

[ "$#" -eq 4 ] || usage
SOURCE="$1"
IMAGE_REF="$2"
ARCH="$3"
RELEASE_TAG="$4"

case "${ARCH}" in
  amd64|arm64) ;;
  *) echo "unsupported arch '${ARCH}' (amd64|arm64)" >&2; exit 2 ;;
esac
case "${IMAGE_REF}" in
  *@sha256:*) echo "image-ref must be a tag, not a digest: ${IMAGE_REF}" >&2; exit 2 ;;
esac

LAYER_LIMIT_BYTES="${LAYER_LIMIT_BYTES:-$((9 * 1024 * 1024 * 1024))}"
CRANE="${CRANE:-crane}"
SOURCE_REVISION="${SOURCE_REVISION:-${GITHUB_SHA:-unknown}}"
WORK_DIR="${WORK_DIR:-$(mktemp -d /tmp/kata-image.XXXXXX)}"
MNT=""
LOOP=""

log() { echo "[kata-image] $*" >&2; }

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "$1 is required" >&2; exit 1; }
}
need "${CRANE}"
need python3
need tar
[ -f "${PLANNER}" ] || { echo "missing ${PLANNER}" >&2; exit 1; }

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  SUDO="sudo -n"
fi

cleanup() {
  local rc=$?
  if [ -n "${MNT}" ]; then
    ${SUDO} umount "${MNT}" 2>/dev/null || true
    rmdir "${MNT}" 2>/dev/null || true
  fi
  if [ -n "${LOOP}" ]; then
    ${SUDO} losetup -d "${LOOP}" 2>/dev/null || true
  fi
  if [ "${KEEP_WORK:-0}" != "1" ]; then
    ${SUDO} rm -rf "${WORK_DIR}"
  else
    log "work dir kept at ${WORK_DIR}"
  fi
  exit "${rc}"
}
trap cleanup EXIT

find_root_partition() {
  local loop="$1" dev
  dev="$(${SUDO} blkid -o device -t LABEL=cloudimg-rootfs "${loop}"p* 2>/dev/null | head -1 || true)"
  if [ -z "${dev}" ]; then
    dev="$(${SUDO} lsblk -bnro PATH,SIZE,FSTYPE "${loop}" | awk '$3=="ext4"{print $2, $1}' | sort -n | tail -1 | awk '{print $2}')"
  fi
  [ -n "${dev}" ] || { echo "no ext4 root partition found on ${loop}" >&2; return 1; }
  echo "${dev}"
}

ROOTFS=""
if [ -d "${SOURCE}" ]; then
  ROOTFS="$(cd "${SOURCE}" && pwd)"
  log "using directory rootfs ${ROOTFS}"
  if [ -z "${SUDO}" ] || [ "$(stat -c %u "${ROOTFS}")" = "$(id -u)" ]; then
    PLAN_SUDO=""
  else
    PLAN_SUDO="${SUDO}"
  fi
else
  [ -f "${SOURCE}" ] || { echo "no such file: ${SOURCE}" >&2; exit 1; }
  need losetup
  need blkid
  need lsblk
  LOOP="$(${SUDO} losetup --find --show --read-only --partscan "${SOURCE}")"
  ${SUDO} partprobe "${LOOP}" 2>/dev/null || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    ls "${LOOP}"p* >/dev/null 2>&1 && break
    sleep 1
  done
  ROOT_PART="$(find_root_partition "${LOOP}")"
  MNT="$(mktemp -d /tmp/kata-rootfs.XXXXXX)"
  ${SUDO} mount -o ro,noload "${ROOT_PART}" "${MNT}"
  ROOTFS="${MNT}"
  PLAN_SUDO="${SUDO}"
  log "mounted ${ROOT_PART} read-only at ${MNT}"
fi

mkdir -p "${WORK_DIR}"
log "planning and packing layers (limit ${LAYER_LIMIT_BYTES} bytes) into ${WORK_DIR}"
${PLAN_SUDO} env "SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-0}" python3 "${PLANNER}" build \
  --rootfs "${ROOTFS}" \
  --work "${WORK_DIR}" \
  --layer-limit "${LAYER_LIMIT_BYTES}" \
  --release-tag "${RELEASE_TAG}" \
  --source-revision "${SOURCE_REVISION}"
if [ -n "${PLAN_SUDO}" ]; then
  ${PLAN_SUDO} chown -R "$(id -u):$(id -g)" "${WORK_DIR}"
fi

CONFIG="${WORK_DIR}/image-config.json"
mapfile -t LAYERS < <(python3 -c 'import json,sys; [print(l) for l in json.load(open(sys.argv[1]))["layers"]]' "${CONFIG}")
mapfile -t ENV_LINES < <(python3 -c 'import json,sys; [print(e) for e in json.load(open(sys.argv[1]))["env"]]' "${CONFIG}")
mapfile -t LABEL_LINES < <(python3 -c 'import json,sys; [print(f"{k}={v}") for k,v in json.load(open(sys.argv[1]))["labels"].items()]' "${CONFIG}")
USER_SPEC="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["user"])' "${CONFIG}")"
WORKDIR_SPEC="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["workdir"])' "${CONFIG}")"

[ "${#LAYERS[@]}" -ge 2 ] || { echo "planner produced ${#LAYERS[@]} layers; expected skeleton + content + adapt" >&2; exit 1; }
for f in "${LAYERS[@]}"; do
  size=$(stat -c %s "${f}")
  [ "${size}" -gt 0 ] || { echo "empty layer ${f}" >&2; exit 1; }
done
log "layers:"
ls -lh "${LAYERS[@]}" >&2

APPEND_ARGS=()
for f in "${LAYERS[@]}"; do APPEND_ARGS+=("--new_layer" "${f}"); done
log "pushing ${#LAYERS[@]} layers to ${IMAGE_REF}"
"${CRANE}" append --oci-empty-base "${APPEND_ARGS[@]}" --new_tag "${IMAGE_REF}" >/dev/null

MUTATE_ARGS=("--user" "${USER_SPEC}" "--workdir" "${WORKDIR_SPEC}" "--set-platform" "linux/${ARCH}")
for e in "${ENV_LINES[@]}"; do MUTATE_ARGS+=("--env" "${e}"); done
for l in "${LABEL_LINES[@]}"; do MUTATE_ARGS+=("--label" "${l}"); done
MUTATE_ARGS+=("--cmd" "/home/runner/run.sh")
log "setting image config"
PUSHED_REF="$("${CRANE}" mutate "${IMAGE_REF}" "${MUTATE_ARGS[@]}" --tag "${IMAGE_REF}")"
DIGEST="${PUSHED_REF##*@}"
case "${DIGEST}" in
  sha256:[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
  *) echo "crane mutate returned no digest: '${PUSHED_REF}'" >&2; exit 1 ;;
esac
[ "${#DIGEST}" -eq 71 ] || { echo "malformed digest '${DIGEST}'" >&2; exit 1; }

REPO="${IMAGE_REF%%:*}"
case "${IMAGE_REF}" in
  */*:*) REPO="${IMAGE_REF%:*}" ;;
esac
PINNED="${REPO}@${DIGEST}"

layer_count="$("${CRANE}" manifest "${PINNED}" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["layers"]))')"
[ "${layer_count}" -eq "${#LAYERS[@]}" ] || { echo "pushed manifest has ${layer_count} layers, expected ${#LAYERS[@]}" >&2; exit 1; }
"${CRANE}" config "${PINNED}" | python3 -c '
import json, sys
cfg = json.load(sys.stdin)
env = dict(e.split("=", 1) for e in cfg["config"]["Env"])
assert cfg["architecture"] == sys.argv[1], cfg["architecture"]
assert cfg["os"] == "linux"
assert cfg["config"]["User"] == sys.argv[2], cfg["config"]["User"]
for k in ("ImageOS", "ImageVersion", "RUNNER_TOOL_CACHE", "HOME", "PATH"):
    assert k in env, k
print("config ok: ImageVersion=%s" % env["ImageVersion"])
' "${ARCH}" "${USER_SPEC}" >&2

log "pushed ${PINNED} (tag ${IMAGE_REF})"
{
  echo "digest=${DIGEST}"
  echo "ref=${PINNED}"
  echo "tag=${IMAGE_REF}"
} | tee -a "${KATA_IMAGE_OUTPUT:-/dev/null}" "${GITHUB_OUTPUT:-/dev/null}"
