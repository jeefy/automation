#!/usr/bin/env bash
# Convert the Packer-built GHA runner VM image (image.raw) into a container
# image for kata-containers runners, so oracle-kata-* jobs get the exact
# toolset of the oracle-* VM runners (same actions/runner-images build).
#
# The rootfs is tens of GB and GHCR rejects layers over 10GB, so the tree is
# greedy-packed into multiple tar layers under LAYER_LIMIT each.
#
# Usage: build-kata-runner-image.sh <image.raw> <image-ref> <arch>
# Example: build-kata-runner-image.sh build/image.raw ghcr.io/cncf/gha-kata-runner:rc-20260901.1 amd64
set -euo pipefail

IMAGE_RAW="${1:?usage: $0 <image.raw> <image-ref> <arch>}"
IMAGE_REF="${2:?usage: $0 <image.raw> <image-ref> <arch>}"
ARCH="${3:?usage: $0 <image.raw> <image-ref> <arch>}"

LAYER_LIMIT=$((9 * 1024 * 1024 * 1024))
MNT="$(mktemp -d /tmp/kata-rootfs.XXXXXX)"
LAYERS_DIR="$(mktemp -d /tmp/kata-layers.XXXXXX)"
LOOP=""

cleanup() {
  sudo umount -R "${MNT}" 2>/dev/null || true
  [ -n "${LOOP}" ] && sudo losetup -d "${LOOP}" 2>/dev/null || true
  sudo rm -rf "${MNT}" "${LAYERS_DIR}"
}
trap cleanup EXIT

command -v crane >/dev/null || { echo "crane is required" >&2; exit 1; }

LOOP="$(sudo losetup -Pf --show "${IMAGE_RAW}")"
sudo partprobe "${LOOP}" || true
sleep 2

# Root partition = the largest one (ubuntu cloudimg: p1 root, p15 ESP, p16 boot)
ROOT_PART="$(lsblk -bnro NAME,SIZE "${LOOP}" | tail -n +2 | sort -k2 -n | tail -1 | awk '{print $1}')"
sudo mount "/dev/${ROOT_PART}" "${MNT}"
echo "Mounted /dev/${ROOT_PART} at ${MNT}"

# Prepare the rootfs for ARC: the VM boots and starts the runner over SSH as
# the ubuntu user; a runner pod instead execs /home/runner/run.sh as uid 1001.
sudo chroot "${MNT}" /bin/bash -e <<'CHROOT'
groupadd -g 1001 runner
useradd -m -u 1001 -g runner -G docker,sudo -s /bin/bash runner
echo "runner ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/runner
tar -xzf /opt/runner-cache/actions-runner-*.tar.gz -C /home/runner
chown -R runner:runner /home/runner
CHROOT

# Runner-images publishes ImageOS/ImageVersion etc. via /etc/environment,
# which PAM applies on VM login but nothing reads in a container - bake them
# into the image config instead.
ENV_ARGS=()
while IFS= read -r line; do
  case "${line}" in
    ImageOS=*|ImageVersion=*|AGENT_TOOLSDIRECTORY=*|RUNNER_TOOL_CACHE=*)
      ENV_ARGS+=("--env" "${line//\"/}")
      ;;
  esac
done < <(sudo cat "${MNT}/etc/environment")
ENV_ARGS+=("--env" "RUNNER_TOOL_CACHE=/opt/hostedtoolcache")
ENV_ARGS+=("--env" "AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache")

# The kata guest boots its own kernel: the host kernel, modules and virtual
# filesystems are dead weight in a container image.
EXCLUDES=(proc sys dev run mnt media tmp boot "lib/modules" "usr/src" "var/cache/apt" "swap.img" "swapfile" "opt/runner-cache" "var/lib/apt/lists" "lost+found")

is_excluded() {
  local rel="$1"
  for ex in "${EXCLUDES[@]}"; do
    [ "${rel}" = "${ex}" ] && return 0
  done
  return 1
}

# Recursively split the tree into units small enough to greedy-pack into
# layers under LAYER_LIMIT.
UNITS_FILE="${LAYERS_DIR}/units.txt"
: > "${UNITS_FILE}"

collect_units() {
  local rel="$1"
  is_excluded "${rel}" && return 0
  local abs="${MNT}/${rel}"
  local size
  size="$(sudo du -sxb "${abs}" 2>/dev/null | awk '{print $1}')"
  if [ "${size}" -le "${LAYER_LIMIT}" ] || [ ! -d "${abs}" ] || [ -L "${abs}" ]; then
    printf '%s\t%s\n' "${size}" "${rel}" >> "${UNITS_FILE}"
    return
  fi
  local child found=0
  while IFS= read -r child; do
    found=1
    collect_units "${rel}/${child}"
  done < <(sudo ls -A "${abs}")
  [ "${found}" -eq 0 ] && printf '%s\t%s\n' "${size}" "${rel}" >> "${UNITS_FILE}"
}

while IFS= read -r entry; do
  collect_units "${entry}"
done < <(sudo ls -A "${MNT}")

# Layer 0 carries the directory skeleton (metadata for every parent dir),
# because splitting means no unit tar includes top-level dirs like /opt with
# their ownership/permissions.
sudo find "${MNT}" -maxdepth 3 -type d -printf '%P\n' | grep -v '^$' > "${LAYERS_DIR}/skeleton.txt"
sudo tar --numeric-owner --xattrs --acls --no-recursion -C "${MNT}" \
  -cf "${LAYERS_DIR}/layer-000.tar" --verbatim-files-from -T "${LAYERS_DIR}/skeleton.txt"

# Greedy-pack units into layers.
layer_idx=1
layer_size=0
layer_list="${LAYERS_DIR}/layer-001.list"
: > "${layer_list}"

flush_layer() {
  [ -s "${layer_list}" ] || return 0
  local tarfile
  tarfile="$(printf '%s/layer-%03d.tar' "${LAYERS_DIR}" "${layer_idx}")"
  echo "Creating $(basename "${tarfile}") (${layer_size} bytes of content)"
  sudo tar --numeric-owner --xattrs --acls -C "${MNT}" \
    -cf "${tarfile}" --verbatim-files-from -T "${layer_list}"
  layer_idx=$((layer_idx + 1))
  layer_size=0
  layer_list="$(printf '%s/layer-%03d.list' "${LAYERS_DIR}" "${layer_idx}")"
  : > "${layer_list}"
}

while IFS=$'\t' read -r size rel; do
  if [ "${layer_size}" -gt 0 ] && [ $((layer_size + size)) -gt "${LAYER_LIMIT}" ]; then
    flush_layer
  fi
  echo "${rel}" >> "${layer_list}"
  layer_size=$((layer_size + size))
done < <(sort -t$'\t' -k1 -rn "${UNITS_FILE}")
flush_layer

LAYER_FILES="$(find "${LAYERS_DIR}" -maxdepth 1 -name 'layer-*.tar' | sort | paste -sd,)"
echo "Assembling ${IMAGE_REF} from:"
ls -lh "${LAYERS_DIR}"/layer-*.tar

crane append --oci-empty-base -t "${IMAGE_REF}" -f "${LAYER_FILES}"

crane mutate "${IMAGE_REF}" \
  --user 1001:1001 \
  --workdir /home/runner \
  "${ENV_ARGS[@]}" \
  --set-platform "linux/${ARCH}" \
  -t "${IMAGE_REF}"

echo "Pushed ${IMAGE_REF}"
crane manifest "${IMAGE_REF}" | head -50
