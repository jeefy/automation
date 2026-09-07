import os
import importlib
import shutil
import subprocess
import tempfile
import sys
import unittest

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
KR = os.path.join(ROOT, "ci/cluster/oke-gha-phx/manifests/kata-runners")
sys.path.insert(0, KR)
render = importlib.import_module("render")


def load(path):
    with open(path) as f:
        return [d for d in yaml.safe_load_all(f) if d]


def ars(docs):
    (a,) = [d for d in docs if d["kind"] == "AutoscalingRunnerSet"]
    return a


PROMOTED = "sha256:" + "a" * 64
LARGE = render.SIZES["8cpu-32gb"]


def rendered_with(digest):
    orig = render.PROMOTED_IMAGE_FILE
    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "IMAGE_DIGEST")
        with open(path, "w") as f:
            f.write(digest + "\n")
        setattr(render, "PROMOTED_IMAGE_FILE", path)
        try:
            prod = dict(render.render_production())
            prepull = render.render_prepull()
        finally:
            setattr(render, "PROMOTED_IMAGE_FILE", orig)
    return prod, prepull


def read(path):
    with open(path) as f:
        return f.read()


def containers(a):
    spec = a["spec"]["template"]["spec"]
    return {c["name"]: c for c in spec["initContainers"] + spec["containers"]}


def qty(s):
    units = {"m": 1e-3, "Ki": 2**10, "Mi": 2**20, "Gi": 2**30, "": 1}
    for u in ("Gi", "Mi", "Ki", "m", ""):
        if str(s).endswith(u):
            return float(str(s)[: len(str(s)) - len(u)] or 0) * units[u]
    raise ValueError(s)


class RenderedManifests(unittest.TestCase):
    def test_committed_files_match_render(self):
        r = subprocess.run([sys.executable, os.path.join(KR, "render.py"), "--check"], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_promotion_pr_flow_renders_and_checks_clean(self):
        with tempfile.TemporaryDirectory() as d:
            shutil.copytree(KR, os.path.join(d, "kata-runners"))
            shutil.copytree(os.path.join(KR, "..", "kata-support"), os.path.join(d, "kata-support"))
            with open(os.path.join(d, "kata-runners", "IMAGE_DIGEST"), "w") as f:
                f.write(PROMOTED + "\n")
            script = os.path.join(d, "kata-runners", "render.py")
            for args in ([], ["--check"]):
                r = subprocess.run([sys.executable, script] + args, capture_output=True, text=True)
                self.assertEqual(r.returncode, 0, r.stderr)
            docs = load(os.path.join(d, "kata-runners", "cncf-kata-8-32-x86.yaml"))
            self.assertEqual(containers(ars(docs))["runner"]["image"], f"ghcr.io/cncf/gha-kata-runner@{PROMOTED}")
            self.assertEqual(load(os.path.join(d, "kata-support", "kata-image-prepull.yaml"))[0]["kind"], "DaemonSet")

    def test_committed_digest_is_sentinel_or_valid_and_files_match_state(self):
        digest = render.promoted_digest()
        kinds = {f: sorted(d["kind"] for d in load(os.path.join(KR, f))) for f in ("cncf-kata-2-8-x86.yaml", "cncf-kata-8-32-x86.yaml")}
        prepull = load(os.path.join(KR, "..", "kata-support", "kata-image-prepull.yaml"))
        if digest is None:
            for f, k in kinds.items():
                self.assertEqual(k, ["Role", "RoleBinding", "ServiceAccount"], f)
            self.assertEqual(prepull, [])
        else:
            for f, k in kinds.items():
                self.assertEqual(k, ["AutoscalingRunnerSet", "Role", "RoleBinding", "ServiceAccount"], f)
            self.assertEqual(prepull[0]["kind"], "DaemonSet")

    def test_unpromoted_state_renders_rbac_only(self):
        prod, prepull = rendered_with(render.UNPROMOTED)
        for f, docs in prod.items():
            self.assertEqual(sorted(d["kind"] for d in docs), ["Role", "RoleBinding", "ServiceAccount"], f)
        self.assertEqual([d for d in yaml.safe_load_all(prepull) if d], [])
        self.assertNotIn("sha256", prepull)

    def test_zero_or_garbage_digest_is_rejected(self):
        for bad in ("sha256:" + "0" * 64, "sha256:abc", "latest-amd64", "ghcr.io/x@sha256:" + "a" * 64):
            with self.assertRaises(SystemExit, msg=bad):
                rendered_with(bad)

    def test_promoted_state(self):
        prod, prepull = rendered_with(PROMOTED)
        image = f"ghcr.io/cncf/gha-kata-runner@{PROMOTED}"
        budgets = {"cncf-kata-2-8-x86.yaml": (2, 8 * 2**30, 1), "cncf-kata-8-32-x86.yaml": (8, 32 * 2**30, 0)}
        for fname, (cpu, mem, min_runners) in budgets.items():
            a = ars(prod[fname])
            self.assertEqual(a["spec"]["minRunners"], min_runners)
            self.assertEqual(a["metadata"]["namespace"], "arc-systems")
            self.check_pod_template(a, image)
            cs = containers(a)
            total_cpu = sum(qty(cs[n]["resources"]["limits"]["cpu"]) for n in ("runner", "dind"))
            total_mem = sum(qty(cs[n]["resources"]["limits"]["memory"]) for n in ("runner", "dind"))
            self.assertAlmostEqual(total_cpu, cpu, msg=fname)
            self.assertEqual(total_mem, mem, fname)
            self.assertIn("cleanup-manager-role-name", str(a["metadata"]["annotations"]))
        ds = [d for d in yaml.safe_load_all(prepull) if d][0]
        self.assertEqual(ds["kind"], "DaemonSet")
        self.assertEqual(ds["metadata"]["namespace"], "arc-systems")
        self.assertEqual(ds["spec"]["template"]["spec"]["initContainers"][0]["image"], image)

    def check_pod_template(self, a, image):
        spec = a["spec"]["template"]["spec"]
        meta = a["spec"]["template"]["metadata"]
        self.assertEqual(meta["annotations"]["cluster-autoscaler.kubernetes.io/safe-to-evict"], "false")
        self.assertEqual(spec["runtimeClassName"], "kata-qemu-runtime-rs")
        self.assertIs(spec["automountServiceAccountToken"], False)
        self.assertIs(spec["enableServiceLinks"], False)
        for v in spec["volumes"]:
            self.assertNotIn("hostPath", v)
        self.assertEqual(spec["nodeSelector"], {"cncf.io/kata-runner": "true"})
        self.assertEqual([c["name"] for c in spec["containers"]], ["runner"])
        for c in spec["initContainers"] + spec["containers"]:
            self.assertEqual(c["image"], image, c["name"])
            self.assertNotIn(":latest", c["image"])
            res = c["resources"]
            self.assertEqual(res["requests"]["cpu"], res["limits"]["cpu"], c["name"])
            self.assertEqual(res["requests"]["memory"], res["limits"]["memory"], c["name"])
        dind = [c for c in spec["initContainers"] if c["name"] == "dind"][0]
        self.assertEqual(dind["restartPolicy"], "Always")
        self.assertEqual(dind["startupProbe"]["exec"]["command"], ["docker", "info"])
        self.assertTrue(dind["securityContext"]["privileged"])
        script = dind["command"][-1]
        self.assertIn("mknod -m 0666 /dev/fuse c 10 229", script)
        self.assertIn("--storage-driver=fuse-overlayfs", script)
        self.assertIn("exec dockerd", script)
        self.assertNotIn("apt-get", script)
        self.assertEqual(spec["volumes"][-1]["emptyDir"]["sizeLimit"], containers(a)["runner"]["resources"]["requests"]["ephemeral-storage"])

    def test_canary_template_is_large_size_in_kata_canary_namespace(self):
        text = read(os.path.join(KR, "canary/canary-template.yaml"))
        self.assertIn(render.CANARY_IMAGE_PLACEHOLDER, text)
        self.assertIn(render.CANARY_LABEL_PLACEHOLDER, text)
        docs = [d for d in yaml.safe_load_all(text.replace(render.CANARY_IMAGE_PLACEHOLDER, "img").replace(render.CANARY_LABEL_PLACEHOLDER, "kata-canary-1-1")) if d]
        self.assertEqual([d["kind"] for d in docs], ["AutoscalingRunnerSet"], "canary renders only the ARS; RBAC is static")
        canary = docs[0]
        self.assertEqual(canary["metadata"]["namespace"], "kata-canary")
        self.assertNotIn("arc-systems", text)
        self.assertNotIn("annotations", canary["metadata"])
        self.assertEqual(canary["spec"]["template"]["spec"]["serviceAccountName"], "kata-canary-runner")
        self.assertEqual(canary["spec"]["minRunners"], 0)
        self.assertLessEqual(canary["spec"]["maxRunners"], 2)
        prod, _ = rendered_with(PROMOTED)
        prod_tpl = ars(prod["cncf-kata-8-32-x86.yaml"])["spec"]["template"]
        canary_tpl = yaml.safe_load(yaml.safe_dump(canary["spec"]["template"]).replace("img", f"ghcr.io/cncf/gha-kata-runner@{PROMOTED}").replace("kata-canary-runner", "oracle-kata-8cpu-32gb-x86-64-gha-rs-no-permission"))
        self.assertEqual(canary_tpl, prod_tpl)

    def test_static_canary_namespace_rbac(self):
        d = os.path.join(ROOT, "ci/cluster/oke-gha-phx/manifests/kata-canary")
        docs = [x for f in os.listdir(d) for x in load(os.path.join(d, f))]
        by = {(x["kind"], x["metadata"]["name"]): x for x in docs}
        self.assertIn(("Namespace", "kata-canary"), by)
        sa = by[("ServiceAccount", "kata-canary-runner")]
        self.assertIs(sa["automountServiceAccountToken"], False)
        rb = by[("RoleBinding", "kata-canary-gha-rs-manager")]
        self.assertEqual(rb["subjects"][0], {"kind": "ServiceAccount", "name": "cncf-gha-rs-controller", "namespace": "arc-systems"})
        es = by[("ExternalSecret", "github-arc-secret")]
        self.assertEqual(es["spec"]["secretStoreRef"]["name"], "oci-secret-store")
        self.assertEqual(es["spec"]["dataFrom"][0]["extract"]["key"], "github-arc-secret")
        for x in docs:
            self.assertEqual(x["metadata"].get("namespace", "kata-canary"), "kata-canary")
        ctrl = load(os.path.join(ROOT, "ci/cluster/oke-gha-phx/manifests/gha-runner-scale-set-controller-values.yaml"))[0]
        self.assertEqual(ctrl["flags"]["watchSingleNamespace"], "", "controller must watch kata-canary too")

    def test_argocd_app_does_not_sync_canary_template(self):
        app = load(os.path.join(ROOT, "ci/cluster/oke-gha-phx/apps/kata-runners.yaml"))[0]
        d = app["spec"]["sources"][0]["directory"]
        self.assertIs(d["recurse"], False)
        self.assertEqual(d["exclude"], "canary/**")

    def test_canary_manager_rbac_is_namespace_scoped_and_minimal(self):
        docs = load(os.path.join(ROOT, "ci/cluster/oke-gha-phx/manifests/kata-canary/manager-rbac.yaml"))
        role = [d for d in docs if d["kind"] == "Role"][0]
        self.assertEqual(role["metadata"]["namespace"], "kata-canary")
        for rule in role["rules"]:
            for forbidden in ("secrets", "roles", "rolebindings", "serviceaccounts", "*"):
                self.assertNotIn(forbidden, rule["resources"])
            if rule["apiGroups"] == [""]:
                self.assertEqual(sorted(rule["verbs"]), ["get", "list", "watch"])
        self.assertFalse([d for d in docs if d["kind"] in ("ClusterRole", "ClusterRoleBinding")])

    def test_kata_deploy_values_target_only_kata_nodes(self):
        v = load(os.path.join(ROOT, "ci/cluster/oke-gha-phx/manifests/kata-deploy-values.yaml"))[0]
        self.assertEqual(v["nodeSelector"], {"cncf.io/kata-runner": "true"})
        self.assertTrue(v["shims"]["disableAll"])
        self.assertTrue(v["shims"]["qemu-runtime-rs"]["enabled"])
        self.assertFalse(v["runtimeClasses"]["createDefault"])
        self.assertEqual(v["updateStrategy"]["type"], "OnDelete")
        self.assertEqual(v["snapshotter"]["setup"], [])


if __name__ == "__main__":
    unittest.main()
