"""Every kubectl call kata-canary.sh makes must be allowed by the manager Role."""

import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
SCRIPT = os.path.join(ROOT, ".github/scripts/kata-canary.sh")
ROLE_FILE = os.path.join(ROOT, "ci/cluster/oke-gha-phx/manifests/kata-canary/manager-rbac.yaml")
IMAGE = "ghcr.io/cncf/gha-kata-runner@sha256:" + "a" * 64
LABEL = "kata-canary-1234-1"

FAKE_KUBECTL = r'''#!/bin/bash
printf '%s\n' "$*" >> "$FAKE_LOG"
case "$*" in
  *" get autoscalingrunnerset kata-canary-1234-1"*) exit 1 ;;
  *" create -f -"*) { echo '---'; cat; } >> "$FAKE_LOG.created" ;;
  *" get ephemeralrunnerset -l "*) echo ephemeralrunnerset/candidate ;;
  *" get pods -l "*" -o name"*) echo pod/candidate ;;
  *jsonpath*) echo Running ;;
esac
exit 0
'''

RESOURCE_GROUP = {
    "autoscalingrunnerset": "actions.github.com", "autoscalingrunnersets": "actions.github.com",
    "ephemeralrunnerset": "actions.github.com", "ephemeralrunner": "actions.github.com",
    "daemonset": "apps", "pods": "", "pod": "", "events": "",
}


def rules():
    role = [d for d in yaml.safe_load_all(Path(ROLE_FILE).read_text()) if d and d["kind"] == "Role"][0]
    allowed = set()
    for r in role["rules"]:
        for g in r["apiGroups"]:
            for res in r["resources"]:
                for v in r["verbs"]:
                    allowed.add((g, res.rstrip("s") if not res.endswith("ss") else res, v))
    return allowed


def norm(res):
    res = res.split("/")[0]
    return res.rstrip("s") if res not in ("pods/log",) else res


class CanaryRbac(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        temporary = tempfile.TemporaryDirectory()
        cls.addClassCleanup(temporary.cleanup)
        cls.tmp = temporary.name
        kubectl = os.path.join(cls.tmp, "kubectl")
        with open(kubectl, "w") as f:
            f.write(FAKE_KUBECTL)
        os.chmod(kubectl, 0o755)
        kc = os.path.join(cls.tmp, "kc")
        open(kc, "w").close()
        cls.env = dict(os.environ, KUBECTL=kubectl, KUBECONFIG=kc, FAKE_LOG=os.path.join(cls.tmp, "log"))
        for cmd in (["create", IMAGE, LABEL], ["wait", LABEL], ["diagnose", LABEL], ["delete", LABEL]):
            r = subprocess.run([SCRIPT] + cmd, env=cls.env, capture_output=True, text=True)
            assert r.returncode == 0, (cmd, r.stderr)
        cls.calls = [shlex.split(l) for l in Path(cls.env["FAKE_LOG"]).read_text().splitlines()]
        cls.created = [d for d in yaml.safe_load_all(Path(cls.env["FAKE_LOG"] + ".created").read_text()) if d]

    def test_only_kata_canary_namespace_and_explicit_kubeconfig(self):
        for c in self.calls:
            self.assertIn("--kubeconfig", c)
            self.assertEqual(c[c.index("--namespace") + 1], "kata-canary", c)

    def test_no_apply_and_no_rbac_or_secret_verbs(self):
        for c in self.calls:
            self.assertNotIn("apply", c, c)
            for forbidden in ("secret", "secrets", "role", "roles", "rolebinding", "serviceaccount"):
                self.assertNotIn(forbidden, c, c)

    def test_every_call_is_allowed_by_manager_role(self):
        allowed = rules()
        for c in self.calls:
            args = [a for a in c if not a.startswith("--")]
            args = [a for i, a in enumerate(c) if not a.startswith("--") and (i == 0 or not c[i - 1] in ("--kubeconfig", "--namespace", "-l", "-o"))]
            verb, resources = args[0], args[1:2]
            if verb == "rollout":
                verb, resources = "get", ["daemonset"]
            if verb == "describe":
                verb, resources = "get", ["pods"]
            if verb == "logs":
                verb, resources = "get", ["pods/log"]
            if verb == "wait":
                for v in ("list", "watch"):
                    self.assertIn(("", resources[0].rstrip("s"), v), allowed, f"wait needs {v} on {resources[0]}: {c}")
                verb = "list"
            if verb == "create":
                resources = [d["kind"].lower() for d in self.created]
            for res in resources:
                res = res.split("/")[0]
                for single in res.split(","):
                    group = RESOURCE_GROUP.get(single.rstrip("s") if single not in ("pods/log",) else single, RESOURCE_GROUP.get(single))
                    self.assertIsNotNone(group, f"unknown resource {single} in {c}")
                    self.assertIn((group, single.rstrip("s"), verb), allowed, f"{verb} {single} not allowed by Role: {c}")

    def test_created_resources(self):
        kinds = [d["kind"] for d in self.created]
        self.assertEqual(sorted(kinds), ["AutoscalingRunnerSet", "DaemonSet"])
        ars = [d for d in self.created if d["kind"] == "AutoscalingRunnerSet"][0]
        ds = [d for d in self.created if d["kind"] == "DaemonSet"][0]
        self.assertEqual(ars["metadata"]["namespace"], "kata-canary")
        self.assertEqual(ars["spec"]["template"]["spec"]["serviceAccountName"], "kata-canary-runner")
        self.assertNotIn("annotations", ars["metadata"])
        self.assertEqual(ars["spec"]["githubConfigSecret"], "github-arc-secret")
        imgs = {c["image"] for c in ars["spec"]["template"]["spec"]["containers"] + ars["spec"]["template"]["spec"]["initContainers"]}
        self.assertEqual(imgs, {IMAGE})
        self.assertEqual(ds["spec"]["template"]["spec"]["initContainers"][0]["image"], IMAGE)
        self.assertEqual(ds["metadata"]["name"], f"{LABEL}-prepull")

    def test_delete_is_bounded_and_verifies_pods_gone(self):
        delete_calls = [c for c in self.calls if "delete" in c or "wait" in c]
        joined = [" ".join(c) for c in delete_calls]
        self.assertTrue(any("delete autoscalingrunnerset kata-canary-1234-1" in j and "--timeout=300s" in j for j in joined), joined)
        self.assertTrue(any(f"delete daemonset {LABEL}-prepull" in j for j in joined), joined)
        self.assertTrue(any("wait pods -l actions.github.com/scale-set-name=kata-canary-1234-1 --for=delete --timeout=300s" in j for j in joined), joined)
        for j in joined:
            self.assertNotIn("--wait=false --timeout", j)
            self.assertNotIn("--force", j)
            self.assertNotIn("--grace-period=0", j)

    def test_rejects_bad_inputs(self):
        for cmd in (["create", "ghcr.io/cncf/gha-kata-runner:latest", LABEL], ["create", IMAGE, "prod-set"], ["create", IMAGE[:-2], LABEL]):
            r = subprocess.run([SCRIPT] + cmd, env=self.env, capture_output=True, text=True)
            self.assertEqual(r.returncode, 2, cmd)
        r = subprocess.run([SCRIPT, "delete", LABEL], env=dict(self.env, KUBECONFIG=""), capture_output=True, text=True)
        self.assertEqual(r.returncode, 2)


if __name__ == "__main__":
    unittest.main()
