"""build-kata-runner-image.sh output plumbing: KATA_IMAGE_OUTPUT and GITHUB_OUTPUT both receive the keys."""

import os
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(os.path.dirname(HERE), "build-kata-runner-image.sh")
sys.path.insert(0, HERE)
from test_kata_rootfs_image import make_fixture  # noqa: E402

DIGEST = "sha256:" + "b" * 64
FAKE_CRANE = f'''#!/bin/bash
printf '%s\\n' "$*" >> "$FAKE_LOG"
case "$1" in
  append) exit 0 ;;
  mutate) tag=""; while [ $# -gt 0 ]; do [ "$1" = "--tag" ] && tag="$2"; shift; done; echo "${{tag%%:*}}@{DIGEST}" ;;
  manifest) LAYERS=$(ls "$FAKE_LAYERS"/layer-* | grep -v '\\.list$' | wc -l); python3 -c "import json;print(json.dumps({{'layers':[{{}}]*$LAYERS}}))" ;;
  config) python3 -c "import json;print(json.dumps({{'architecture':'amd64','os':'linux','config':{{'User':'1001','Env':['ImageOS=ubuntu24','ImageVersion=1','RUNNER_TOOL_CACHE=/opt/hostedtoolcache','HOME=/home/runner','PATH=/usr/bin']}}}}))" ;;
  *) echo "unexpected crane $*" >&2; exit 1 ;;
esac
'''


@unittest.skipUnless(shutil.which("tar"), "tar required")
class BuildScriptOutputs(unittest.TestCase):
    def test_outputs_written_to_both_files(self):
        tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, tmp, True)
        rootfs = os.path.join(tmp, "rootfs")
        make_fixture(rootfs)
        crane = os.path.join(tmp, "crane")
        with open(crane, "w") as f:
            f.write(FAKE_CRANE)
        os.chmod(crane, 0o755)
        work = os.path.join(tmp, "work")
        out = os.path.join(tmp, "out.txt")
        gh_out = os.path.join(tmp, "github_output")
        with open(gh_out, "w") as f:
            f.write("previous=kept\n")
        env = dict(os.environ, CRANE=crane, FAKE_LOG=os.path.join(tmp, "crane.log"), FAKE_LAYERS=os.path.join(work, "layers"),
                   WORK_DIR=work, KEEP_WORK="1", LAYER_LIMIT_BYTES="16384", KATA_IMAGE_OUTPUT=out, GITHUB_OUTPUT=gh_out,
                   SOURCE_REVISION="feedface")
        ref = "ghcr.io/cncf/gha-kata-runner:rc-test-amd64"
        r = subprocess.run([SCRIPT, rootfs, ref, "amd64", "ubuntu24/20260901.1"], env=env, capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        expected = {"digest": DIGEST, "ref": f"ghcr.io/cncf/gha-kata-runner@{DIGEST}", "tag": ref}
        kv = {}
        for path in (out, gh_out):
            with open(path) as f:
                kv = dict(l.split("=", 1) for l in f.read().splitlines() if "=" in l)
            for k, v in expected.items():
                self.assertEqual(kv.get(k), v, f"{k} in {path}")
        self.assertEqual(kv["previous"], "kept", "GITHUB_OUTPUT must be appended, not truncated")
        self.assertFalse(os.path.exists(os.path.join(os.getcwd(), ">>" + gh_out)))
        with open(env["FAKE_LOG"]) as f:
            log = f.read()
        self.assertIn("--oci-empty-base", log)
        self.assertNotIn("--entrypoint", log)
        self.assertIn("--set-platform linux/amd64", log)
        self.assertIn("--cmd /home/runner/run.sh", log)


if __name__ == "__main__":
    unittest.main()
