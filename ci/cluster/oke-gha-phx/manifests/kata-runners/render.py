#!/usr/bin/env python3
"""Render the kata runner AutoscalingRunnerSet manifests from one template.

Rendered files are committed (ArgoCD syncs plain YAML); this script keeps the
two production sizes and the canary set structurally identical. Run with
--check in CI/tests to fail when a committed file drifts from the template.

    python3 ci/cluster/oke-gha-phx/manifests/kata-runners/render.py          # rewrite
    python3 ci/cluster/oke-gha-phx/manifests/kata-runners/render.py --check  # verify
"""

from __future__ import annotations

import argparse
import copy
import os
import sys

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
NAMESPACE = "arc-systems"
CONTROLLER_SA = "cncf-gha-rs-controller"
CHART = "gha-rs-0.14.1"
RUNTIME_CLASS = "kata-qemu-runtime-rs"
NODE_LABEL = "cncf.io/kata-runner"
PROMOTED_IMAGE_FILE = os.path.join(HERE, "IMAGE_DIGEST")
IMAGE_REPO = "ghcr.io/cncf/gha-kata-runner"
CANARY_IMAGE_PLACEHOLDER = "__KATA_CANDIDATE_IMAGE__"
CANARY_LABEL_PLACEHOLDER = "__KATA_CANARY_LABEL__"

# Guaranteed QoS: requests == limits on every container, so the pod's host
# cgroup, the kata guest sizing (sum of container limits) and the scheduler
# all agree on one job budget. cpu is Kubernetes CPU = one hardware thread.
SIZES = {
    "2cpu-8gb": {
        "file": "cncf-kata-2-8-x86.yaml",
        "min_runners": 1,
        "max_runners": 8,
        "runner": {"cpu": "750m", "memory": "3Gi"},
        "dind": {"cpu": "1250m", "memory": "5Gi"},
        "scratch": "40Gi",
        "scratch_headroom": "44Gi",
    },
    "8cpu-32gb": {
        "file": "cncf-kata-8-32-x86.yaml",
        "min_runners": 0,
        "max_runners": 6,
        "runner": {"cpu": "3", "memory": "10Gi"},
        "dind": {"cpu": "5", "memory": "22Gi"},
        "scratch": "100Gi",
        "scratch_headroom": "110Gi",
    },
}

METRIC_LABELS = ["repository", "organization", "enterprise", "job_name", "event_name"]
GAUGE_LABELS = ["name", "namespace", "repository", "organization", "enterprise"]
BUCKETS = [0.01, 0.05, 0.1, 0.5, 1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0, 10.0, 12.0, 15.0, 18.0,
           20.0, 25.0, 30.0, 40.0, 50.0, 60.0, 70.0, 80.0, 90.0, 100.0, 110.0, 120.0, 150.0, 180.0,
           210.0, 240.0, 300.0, 360.0, 420.0, 480.0, 540.0, 600.0, 900.0, 1200.0, 1800.0, 2400.0,
           3000.0, 3600.0]

DIND_COMMAND = (
    "[ -e /dev/fuse ] || mknod -m 0666 /dev/fuse c 10 229\n"
    "exec dockerd --host=unix:///var/run/docker.sock --group=docker --data-root=/docker/ "
    "--storage-driver=fuse-overlayfs --mtu=1400 --default-network-opt=bridge=com.docker.network.driver.mtu=1400\n"
)

WORK_MOUNTS = [
    ("/home/runner/_work", "_work"),
    ("/home/runner/.cache", ".cache"),
    ("/home/runner/.gradle", ".gradle"),
    ("/home/runner/go", "go"),
    ("/home/runner/.m2", ".m2"),
    ("/tmp", "tmp"),
]


UNPROMOTED = "unpromoted"
CANARY_NAMESPACE = "kata-canary"
CANARY_RUNNER_SA = "kata-canary-runner"


def promoted_digest() -> str | None:
    with open(PROMOTED_IMAGE_FILE) as f:
        digest = f.read().strip()
    if digest == UNPROMOTED:
        return None
    if not digest.startswith("sha256:") or len(digest) != 71 or not all(c in "0123456789abcdef" for c in digest[7:]):
        raise SystemExit(f"{PROMOTED_IMAGE_FILE} must contain '{UNPROMOTED}' or a single sha256 digest, got {digest!r}")
    if digest[7:] == "0" * 64:
        raise SystemExit(f"{PROMOTED_IMAGE_FILE}: an all-zero digest is not a real image; use '{UNPROMOTED}'")
    return digest


def promoted_image() -> str | None:
    d = promoted_digest()
    return f"{IMAGE_REPO}@{d}" if d else None


def labels(name: str, extra: dict | None = None, namespace: str = NAMESPACE) -> dict:
    out = {
        "helm.sh/chart": CHART,
        "app.kubernetes.io/name": name,
        "app.kubernetes.io/instance": name,
        "app.kubernetes.io/version": "0.14.1",
        "app.kubernetes.io/managed-by": "Helm",
        "app.kubernetes.io/part-of": "gha-rs",
        "actions.github.com/scale-set-name": name,
        "actions.github.com/scale-set-namespace": namespace,
    }
    out.update(extra or {})
    return out


def rbac(name: str) -> list:
    sa = f"{name}-gha-rs-no-permission"
    role = f"{name}-gha-rs-manager"
    return [
        {
            "apiVersion": "v1",
            "kind": "ServiceAccount",
            "metadata": {"name": sa, "namespace": NAMESPACE, "labels": labels(name),
                         "finalizers": ["actions.github.com/cleanup-protection"]},
            "automountServiceAccountToken": False,
        },
        {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "Role",
            "metadata": {"name": role, "namespace": NAMESPACE,
                         "labels": labels(name, {"app.kubernetes.io/component": "manager-role"}),
                         "finalizers": ["actions.github.com/cleanup-protection"]},
            "rules": [
                {"apiGroups": [""], "resources": ["pods"], "verbs": ["create", "delete", "get"]},
                {"apiGroups": [""], "resources": ["pods/status"], "verbs": ["get"]},
                {"apiGroups": [""], "resources": ["secrets"],
                 "verbs": ["create", "delete", "get", "list", "patch", "update"]},
                {"apiGroups": [""], "resources": ["serviceaccounts"],
                 "verbs": ["create", "delete", "get", "list", "patch", "update"]},
                {"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["rolebindings"],
                 "verbs": ["create", "delete", "get", "patch", "update"]},
                {"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["roles"],
                 "verbs": ["create", "delete", "get", "patch", "update"]},
            ],
        },
        {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "RoleBinding",
            "metadata": {"name": role, "namespace": NAMESPACE,
                         "labels": labels(name, {"app.kubernetes.io/component": "manager-role-binding"}),
                         "finalizers": ["actions.github.com/cleanup-protection"]},
            "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": role},
            "subjects": [{"kind": "ServiceAccount", "name": CONTROLLER_SA, "namespace": NAMESPACE}],
        },
    ]


def work_mounts() -> list:
    return [{"mountPath": mp, "name": "work", "subPath": sp} for mp, sp in WORK_MOUNTS]


def resources(spec: dict, scratch_req: str, scratch_lim: str) -> dict:
    return {
        "requests": {"cpu": spec["cpu"], "memory": spec["memory"], "ephemeral-storage": scratch_req},
        "limits": {"cpu": spec["cpu"], "memory": spec["memory"], "ephemeral-storage": scratch_lim},
    }


def pod_template(size: dict, image: str, sa: str) -> dict:
    runner = {
        "name": "runner",
        "image": image,
        "imagePullPolicy": "IfNotPresent",
        "command": ["/home/runner/run.sh"],
        "env": [
            {"name": "DOCKER_HOST", "value": "unix:///var/run/docker.sock"},
            {"name": "RUNNER_WAIT_FOR_DOCKER_IN_SECONDS", "value": "120"},
        ],
        "resources": resources(size["runner"], size["scratch"], size["scratch_headroom"]),
        "securityContext": {"allowPrivilegeEscalation": True, "runAsUser": 1001, "runAsGroup": 1001},
        "volumeMounts": work_mounts() + [{"mountPath": "/var/run", "name": "dind-sock"}],
    }
    dind = {
        "name": "dind",
        "image": image,
        "imagePullPolicy": "IfNotPresent",
        "restartPolicy": "Always",
        "command": ["bash", "-ec", DIND_COMMAND],
        "securityContext": {"privileged": True, "runAsUser": 0},
        "startupProbe": {
            "exec": {"command": ["docker", "info"]},
            "initialDelaySeconds": 0,
            "periodSeconds": 5,
            "failureThreshold": 24,
        },
        "resources": resources(size["dind"], "1Gi", size["scratch_headroom"]),
        "volumeMounts": work_mounts() + [
            {"mountPath": "/var/run", "name": "dind-sock"},
            {"mountPath": "/home/runner/externals", "name": "dind-externals"},
            {"mountPath": "/docker", "name": "work", "subPath": "docker"},
        ],
    }
    init_externals = {
        "name": "init-dind-externals",
        "image": image,
        "imagePullPolicy": "IfNotPresent",
        "command": ["cp"],
        "args": ["-r", "/home/runner/externals/.", "/home/runner/tmpDir/"],
        "resources": {"requests": {"cpu": "250m", "memory": "256Mi"},
                      "limits": {"cpu": "250m", "memory": "256Mi"}},
        "volumeMounts": [{"mountPath": "/home/runner/tmpDir", "name": "dind-externals"}],
    }
    init_tmp = {
        "name": "init-scratch-perms",
        "image": image,
        "imagePullPolicy": "IfNotPresent",
        "command": ["sh", "-ec", "chmod 1777 /scratch/tmp && chown 1001:1001 /scratch/_work /scratch/.cache /scratch/.gradle /scratch/go /scratch/.m2 && mkdir -p /scratch/docker"],
        "securityContext": {"runAsUser": 0, "capabilities": {"drop": ["ALL"], "add": ["CHOWN", "FOWNER", "DAC_OVERRIDE"]}},
        "resources": {"requests": {"cpu": "100m", "memory": "64Mi"},
                      "limits": {"cpu": "100m", "memory": "64Mi"}},
        "volumeMounts": [{"mountPath": f"/scratch/{sp}", "name": "work", "subPath": sp} for _, sp in WORK_MOUNTS],
    }
    return {
        "metadata": {
            "labels": {"cncf.io/runner-kind": "kata"},
            "annotations": {"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"},
        },
        "spec": {
            "runtimeClassName": RUNTIME_CLASS,
            "automountServiceAccountToken": False,
            "enableServiceLinks": False,
            "serviceAccountName": sa,
            "restartPolicy": "Never",
            "terminationGracePeriodSeconds": 60,
            "securityContext": {"fsGroup": 1001},
            "initContainers": [init_tmp, init_externals, dind],
            "containers": [runner],
            "volumes": [
                {"name": "dind-sock", "emptyDir": {}},
                {"name": "dind-externals", "emptyDir": {"sizeLimit": "1Gi"}},
                {"name": "work", "emptyDir": {"sizeLimit": size["scratch"]}},
            ],
            "nodeSelector": {NODE_LABEL: "true"},
            "tolerations": [{"key": NODE_LABEL, "operator": "Exists", "effect": "NoSchedule"}],
        },
    }


def listener_metrics() -> dict:
    return {
        "counters": {
            "gha_started_jobs_total": {"labels": METRIC_LABELS},
            "gha_completed_jobs_total": {"labels": METRIC_LABELS + ["job_result"]},
        },
        "gauges": {g: {"labels": GAUGE_LABELS} for g in (
            "gha_assigned_jobs", "gha_running_jobs", "gha_registered_runners", "gha_busy_runners",
            "gha_min_runners", "gha_max_runners", "gha_desired_runners", "gha_idle_runners")},
        "histograms": {
            "gha_job_startup_duration_seconds": {"labels": METRIC_LABELS, "buckets": BUCKETS},
            "gha_job_execution_duration_seconds": {"labels": METRIC_LABELS + ["job_result"], "buckets": BUCKETS},
        },
    }


def autoscaling_runner_set(name: str, namespace: str, sa: str, size: dict, image: str,
                           min_runners: int, max_runners: int, cleanup_annotations: bool) -> dict:
    role = f"{name}-gha-rs-manager"
    meta = {
        "name": name,
        "namespace": namespace,
        "labels": labels(name, {"app.kubernetes.io/component": "autoscaling-runner-set"}, namespace),
    }
    if cleanup_annotations:
        meta["annotations"] = {
            "actions.github.com/cleanup-manager-role-binding": role,
            "actions.github.com/cleanup-manager-role-name": role,
            "actions.github.com/cleanup-no-permission-service-account-name": sa,
        }
    return {
        "apiVersion": "actions.github.com/v1alpha1",
        "kind": "AutoscalingRunnerSet",
        "metadata": meta,
        "spec": {
            "githubConfigUrl": "https://github.com/enterprises/cncf",
            "githubConfigSecret": "github-arc-secret",
            "minRunners": min_runners,
            "maxRunners": max_runners,
            "listenerMetrics": listener_metrics(),
            "listenerTemplate": {"spec": {"containers": [{"name": "listener", "securityContext": {"runAsUser": 1000}}]}},
            "template": pod_template(size, image, sa),
        },
    }


def scale_set(name: str, size: dict, image: str | None, min_runners: int, max_runners: int) -> list:
    sa = f"{name}-gha-rs-no-permission"
    docs = rbac(name)
    if image is not None:
        docs.append(autoscaling_runner_set(name, NAMESPACE, sa, size, image, min_runners, max_runners, True))
    return docs


def render_production() -> dict[str, list]:
    image = promoted_image()
    out = {}
    for key, size in SIZES.items():
        name = f"oracle-kata-{key}-x86-64"
        out[size["file"]] = scale_set(name, size, image, size["min_runners"], size["max_runners"])
    return out


PREPULL_FILE = os.path.join(HERE, "..", "kata-support", "kata-image-prepull.yaml")


def render_prepull() -> str:
    image = promoted_image()
    header = "# Generated by render.py from IMAGE_DIGEST - do not edit by hand.\n"
    if image is None:
        return header + "# IMAGE_DIGEST is 'unpromoted': no image to pre-pull, so no DaemonSet is rendered.\n"
    return header + yaml.safe_dump(prepull_daemonset("kata-image-prepull", NAMESPACE, image, {}),
                                   sort_keys=False, default_flow_style=False, width=120)


def prepull_daemonset(name: str, namespace: str, image: str, extra_labels: dict) -> dict:
    lbl = {"app.kubernetes.io/name": name, **extra_labels}
    return {
        "apiVersion": "apps/v1",
        "kind": "DaemonSet",
        "metadata": {"name": name, "namespace": namespace, "labels": lbl},
        "spec": {
            "selector": {"matchLabels": {"app.kubernetes.io/name": name}},
            "updateStrategy": {"type": "RollingUpdate", "rollingUpdate": {"maxUnavailable": 1}},
            "template": {
                "metadata": {"labels": lbl},
                "spec": {
                    "automountServiceAccountToken": False,
                    "nodeSelector": {NODE_LABEL: "true"},
                    "tolerations": [{"key": NODE_LABEL, "operator": "Exists", "effect": "NoSchedule"}],
                    "priorityClassName": "system-node-critical",
                    "initContainers": [{
                        "name": "prepull",
                        "image": image,
                        "imagePullPolicy": "IfNotPresent",
                        "command": ["/bin/true"],
                        "resources": {"requests": {"cpu": "10m", "memory": "16Mi"}, "limits": {"cpu": "10m", "memory": "16Mi"}},
                        "securityContext": {"runAsUser": 1001, "allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
                    }],
                    "containers": [{
                        "name": "pause",
                        "image": "registry.k8s.io/pause:3.10",
                        "resources": {"requests": {"cpu": "5m", "memory": "8Mi"}, "limits": {"cpu": "5m", "memory": "8Mi"}},
                        "securityContext": {"runAsNonRoot": True, "runAsUser": 65535, "allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
                    }],
                },
            },
        },
    }


def render_canary() -> list:
    size = copy.deepcopy(SIZES["8cpu-32gb"])
    return [autoscaling_runner_set(CANARY_LABEL_PLACEHOLDER, CANARY_NAMESPACE, CANARY_RUNNER_SA, size,
                                   CANARY_IMAGE_PLACEHOLDER, 0, 2, False)]


def dump(docs: list, header: str = "") -> str:
    return "# Generated by render.py - do not edit by hand.\n" + header + "---\n" + "---\n".join(
        yaml.safe_dump(d, sort_keys=False, default_flow_style=False, width=120) for d in docs)


def main(argv=None) -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--check", action="store_true")
    p.add_argument("--canary-manifest", nargs=3, metavar=("IMAGE", "LABEL", "MODE"),
                   help="print the canary ARS (MODE=ars) or its pre-pull DaemonSet (MODE=prepull) for one candidate")
    a = p.parse_args(argv)
    if a.canary_manifest:
        image, label, mode = a.canary_manifest
        if mode == "ars":
            size = copy.deepcopy(SIZES["8cpu-32gb"])
            print(yaml.safe_dump(autoscaling_runner_set(label, CANARY_NAMESPACE, CANARY_RUNNER_SA, size, image, 0, 2, False), sort_keys=False, width=120))
        elif mode == "prepull":
            print(yaml.safe_dump(prepull_daemonset(f"{label}-prepull", CANARY_NAMESPACE, image, {"cncf.io/kata-canary": label}), sort_keys=False, width=120))
        else:
            raise SystemExit("MODE must be ars or prepull")
        return 0
    header = ""
    if promoted_digest() is None:
        header = ("# IMAGE_DIGEST is 'unpromoted': only RBAC is rendered, no AutoscalingRunnerSet exists until\n"
                  "# promote-kata-image.yml opens the first IMAGE_DIGEST PR with a canary-tested digest.\n")
    files = {os.path.join(HERE, f): dump(docs, header) for f, docs in render_production().items()}
    files[os.path.join(HERE, "canary", "canary-template.yaml")] = dump(render_canary())
    files[PREPULL_FILE] = render_prepull()
    drift = []
    for path, content in files.items():
        if a.check:
            try:
                with open(path) as f:
                    if f.read() != content:
                        drift.append(path)
            except FileNotFoundError:
                drift.append(path)
        else:
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "w") as f:
                f.write(content)
    if drift:
        print("rendered manifests drifted from render.py:\n  " + "\n  ".join(drift), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
