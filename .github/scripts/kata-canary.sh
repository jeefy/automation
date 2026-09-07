#!/usr/bin/env bash
# Manage a temporary kata canary AutoscalingRunnerSet for one candidate image.
#
#   kata-canary.sh create  <candidate-image@sha256:...> <canary-label>
#   kata-canary.sh wait    <canary-label>
#   kata-canary.sh delete  <canary-label>
#
# Talks to the cluster through KUBECONFIG only (no default context is ever
# assumed) with the kata-canary-manager ServiceAccount from
# ci/cluster/oke-gha-phx/manifests/kata-canary/manager-rbac.yaml. Per-run
# resources are only an AutoscalingRunnerSet and a pre-pull DaemonSet in the
# kata-canary namespace, created with `kubectl create` (no apply/patch); the
# runner ServiceAccount and controller Role are static (runner-rbac.yaml).
# The canary label is the runs-on label the test job uses; it is unique per
# workflow run so a stale set can never pick up a newer run's job.
#
# Environment:
#   KUBECONFIG            required, path to the narrowly scoped kubeconfig
#   CANARY_NAMESPACE      default kata-canary
#   CANARY_RENDER         default ci/cluster/oke-gha-phx/manifests/kata-runners/render.py
#   CANARY_READY_TIMEOUT  seconds to wait for the listener + image pre-pull (default 1500)
#   KUBECTL               kubectl binary (default kubectl)
set -euo pipefail

CMD="${1:?usage: $0 create|wait|diagnose|delete ...}"
NS="${CANARY_NAMESPACE:-kata-canary}"
KUBECTL="${KUBECTL:-kubectl}"
RENDER="${CANARY_RENDER:-$(dirname "${BASH_SOURCE[0]}")/../../ci/cluster/oke-gha-phx/manifests/kata-runners/render.py}"
READY_TIMEOUT="${CANARY_READY_TIMEOUT:-1500}"

[ -n "${KUBECONFIG:-}" ] || { echo "KUBECONFIG must be set explicitly" >&2; exit 2; }
[ -r "${KUBECONFIG}" ] || { echo "KUBECONFIG ${KUBECONFIG} is not readable" >&2; exit 2; }

k() { "${KUBECTL}" --kubeconfig "${KUBECONFIG}" --namespace "${NS}" --request-timeout=60s "$@"; }

validate_label() {
  case "$1" in
    kata-canary-[a-z0-9][a-z0-9-]*) ;;
    *) echo "canary label '$1' must match kata-canary-<run-id>-<attempt>" >&2; exit 2 ;;
  esac
  [ "${#1}" -le 45 ] || { echo "canary label too long" >&2; exit 2; }
}

validate_image() {
  case "$1" in
    ghcr.io/*/gha-kata-runner@sha256:*) ;;
    *) echo "candidate image must be a ghcr.io gha-kata-runner digest reference: $1" >&2; exit 2 ;;
  esac
  local digest="${1##*@}"
  [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "malformed digest in $1" >&2; exit 2; }
}

render() { python3 "${RENDER}" --canary-manifest "$1" "$2" "$3"; }

case "${CMD}" in
  render)
    validate_image "${2:?image}"; validate_label "${3:?label}"
    render "$2" "$3" ars; echo '---'; render "$2" "$3" prepull
    ;;
  create)
    IMAGE="${2:?image}"; LABEL="${3:?label}"
    validate_image "${IMAGE}"; validate_label "${LABEL}"
    if k get autoscalingrunnerset "${LABEL}" >/dev/null 2>&1; then
      echo "canary set ${LABEL} already exists" >&2; exit 1
    fi
    render "${IMAGE}" "${LABEL}" prepull | k create -f -
    render "${IMAGE}" "${LABEL}" ars | k create -f -
    ;;
  wait)
    LABEL="${2:?label}"
    validate_label "${LABEL}"
    echo "waiting up to ${READY_TIMEOUT}s for image pre-pull on every kata node"
    k rollout status "daemonset/${LABEL}-prepull" --timeout="${READY_TIMEOUT}s"
    echo "waiting for the reconciled ephemeral runner set of ${LABEL}"
    deadline=$(( $(date +%s) + 600 ))
    until [ -n "$(k get ephemeralrunnerset -l "actions.github.com/scale-set-name=${LABEL}" -o name)" ]; do
      [ "$(date +%s)" -lt "${deadline}" ] || { echo "runner set for ${LABEL} not reconciled" >&2; k get autoscalingrunnerset "${LABEL}" -o yaml | sed -n '/^status:/,$p' >&2; exit 1; }
      sleep 10
    done
    echo "canary ${LABEL} ready"
    ;;
  diagnose)
    LABEL="${2:?label}"
    validate_label "${LABEL}"
    k get autoscalingrunnerset,ephemeralrunnerset,ephemeralrunner -l "actions.github.com/scale-set-name=${LABEL}" -o wide || true
    k get daemonset "${LABEL}-prepull" -o wide || true
    k get pods -o wide || true
    for p in $(k get pods -l "actions.github.com/scale-set-name=${LABEL}" -o name); do
      echo "== ${p}"; k describe "${p}" | sed -n '/^Events:/,$p' || true
      k logs "${p}" --all-containers --tail=200 --prefix || true
    done
    k get events --sort-by=.lastTimestamp | tail -50 || true
    ;;
  delete)
    LABEL="${2:?label}"
    validate_label "${LABEL}"
    SELECTOR="actions.github.com/scale-set-name=${LABEL}"
    k delete daemonset "${LABEL}-prepull" --ignore-not-found --wait=false
    if ! k delete autoscalingrunnerset "${LABEL}" --ignore-not-found --wait=true --timeout=300s; then
      echo "autoscalingrunnerset ${LABEL} did not finish deleting within 300s (controller finalizers?)" >&2
      k get autoscalingrunnerset,ephemeralrunnerset,ephemeralrunner -l "${SELECTOR}" -o wide >&2 || true
    fi
    echo "waiting for runner pods of ${LABEL} to disappear"
    if [ -n "$(k get pods -l "${SELECTOR}" -o name)" ] && ! k wait pods -l "${SELECTOR}" --for=delete --timeout=300s; then
      echo "runner pods of ${LABEL} still present after 300s:" >&2
      k get pods -l "${SELECTOR}" -o wide >&2 || true
      exit 1
    fi
    remaining="$(k get autoscalingrunnerset -l "${SELECTOR}" -o name)"
    [ -z "${remaining}" ] || { echo "leftover: ${remaining}" >&2; exit 1; }
    echo "canary ${LABEL} cleaned up"
    ;;
  *)
    echo "unknown command ${CMD}" >&2; exit 2 ;;
esac
