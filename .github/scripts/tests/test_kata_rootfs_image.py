"""Tests for kata_rootfs_image.py using an unprivileged fixture rootfs.

Run: python3 -m unittest discover -s .github/scripts/tests -v
"""

import io
import json
import os
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

import kata_rootfs_image as kri  # noqa: E402

SMALL_LIMIT = 16 * 1024


def write(path, data: bytes | str = b"", mode=0o644):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(data if isinstance(data, bytes) else data.encode())
    os.chmod(path, mode)


def make_runner_tarball(path):
    with tarfile.open(path, "w:gz") as tf:
        for name, data, mode in (
            ("./run.sh", b"#!/bin/bash\nexec ./bin/Runner.Listener run \"$@\"\n", 0o755),
            ("./config.sh", b"#!/bin/bash\n", 0o755),
            ("./bin/Runner.Listener", b"\x7fELF", 0o755),
            ("./externals/node20/bin/node", b"\x7fELF", 0o755),
        ):
            ti = tarfile.TarInfo(name)
            ti.size = len(data)
            ti.mode = mode
            ti.uid, ti.gid = 1000, 1000
            ti.uname, ti.gname = "ubuntu", "ubuntu"
            tf.addfile(ti, io.BytesIO(data))
        ti = tarfile.TarInfo("./bin")
        ti.type = tarfile.DIRTYPE
        ti.mode = 0o755
        tf.addfile(ti)


def make_fixture(root):
    """A miniature Ubuntu 24.04 runner-images rootfs."""
    write(f"{root}/etc/environment",
          'PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"\n'
          "ImageVersion=20260901.1.0\nImageOS=ubuntu24\nACCEPT_EULA=Y\n"
          "XDG_CONFIG_HOME=$HOME/.config\nAGENT_TOOLSDIRECTORY=/opt/hostedtoolcache\n"
          "RUNNER_TOOL_CACHE=/opt/hostedtoolcache\nANDROID_SDK_ROOT=/usr/local/lib/android/sdk\n"
          "JAVA_TOOL_OPTIONS=-Da=1,-Db=2\n")
    write(f"{root}/etc/default/locale", "LANG=C.UTF-8\n")
    write(f"{root}/etc/passwd",
          "root:x:0:0:root:/root:/bin/bash\nubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n")
    write(f"{root}/etc/group",
          "root:x:0:\nadm:x:4:syslog,ubuntu\nsudo:x:27:ubuntu\nshadow:x:42:\nubuntu:x:1000:\ndocker:x:988:ubuntu\n")
    write(f"{root}/etc/shadow",
          "root:*:19000:0:99999:7:::\nubuntu:$6$abc$hash:19000:0:99999:7:::\n", 0o640)
    write(f"{root}/etc/gshadow", "root:*::\nadm:*::syslog,ubuntu\nsudo:*::ubuntu\ndocker:!::ubuntu\n", 0o640)
    write(f"{root}/etc/subuid", "ubuntu:100000:65536\n")
    write(f"{root}/etc/subgid", "ubuntu:100000:65536\n")
    write(f"{root}/etc/skel/.bashrc", "# bashrc\n")
    write(f"{root}/etc/skel/.config/configstore/.keep", "")
    write(f"{root}/etc/ssh/ssh_host_ed25519_key", "PRIVATE", 0o600)
    write(f"{root}/etc/ssh/sshd_config", "Port 22\n")
    write(f"{root}/etc/machine-id", "deadbeef\n")
    write(f"{root}/etc/hostname", "packer\n")
    os.makedirs(f"{root}/etc/sudoers.d", exist_ok=True)
    write(f"{root}/etc/sudoers.d/90-cloud-init-users", "ubuntu ALL=(ALL) NOPASSWD:ALL\n", 0o440)
    os.makedirs(f"{root}/run/systemd/resolve", exist_ok=True)
    os.symlink("../run/systemd/resolve/stub-resolv.conf", f"{root}/etc/resolv.conf")

    write(f"{root}/usr/bin/dockerd", b"\x7fELF" * 100, 0o755)
    write(f"{root}/usr/bin/fuse-overlayfs", b"\x7fELF" * 50, 0o755)
    write(f"{root}/usr/bin/sudo", b"\x7fELF" * 50, 0o4755)
    write(f"{root}/usr/bin/tool with space", b"x" * 300, 0o755)
    write(f"{root}/usr/bin/weird\nname", b"y" * 10, 0o755)
    write(f"{root}/usr/lib/modules/6.8.0-oracle/kernel/mod.ko", b"\x00" * 5000)
    write(f"{root}/usr/src/linux-headers-6.8/Makefile", b"all:\n")
    os.symlink("usr/bin", f"{root}/bin")
    os.symlink("usr/lib", f"{root}/lib")
    os.symlink("usr/sbin", f"{root}/sbin")
    for i in range(6):
        write(f"{root}/opt/hostedtoolcache/Python/3.12.{i}/x64/bin/python3", os.urandom(2500), 0o755)
    os.chmod(f"{root}/opt/hostedtoolcache", 0o777)
    os.chmod(f"{root}/opt/hostedtoolcache/Python/3.12.3", 0o777)
    for i in range(3):
        write(f"{root}/usr/share/dotnet/sdk/{i}/lib.dll", os.urandom(3000))
    write(f"{root}/usr/share/doc/a/copyright", "c" * 100)
    os.link(f"{root}/usr/share/doc/a/copyright", f"{root}/usr/share/doc/a/copyright.link")
    write(f"{root}/swap.img", b"\x00" * 9000)
    os.makedirs(f"{root}/lost+found", exist_ok=True)
    write(f"{root}/home/ubuntu/.bash_history", "secret\n")
    write(f"{root}/home/ubuntu/.ssh/authorized_keys", "ssh-ed25519 AAAA\n", 0o600)
    write(f"{root}/home/ubuntu/.profile", "profile\n")
    write(f"{root}/root/.bash_history", "secret\n")
    write(f"{root}/var/cache/apt/pkgcache.bin", b"z" * 4000)
    write(f"{root}/var/lib/apt/lists/archive_Release", b"r" * 100)
    write(f"{root}/var/lib/cloud/instances/i-1/user-data.txt", "password: ubuntu\n")
    write(f"{root}/var/log/cloud-init.log", "log\n")
    write(f"{root}/var/lib/dpkg/status", "Package: bash\n")
    os.makedirs(f"{root}/proc", exist_ok=True)
    os.makedirs(f"{root}/sys", exist_ok=True)
    os.makedirs(f"{root}/dev", exist_ok=True)
    os.makedirs(f"{root}/tmp", exist_ok=True)
    os.chmod(f"{root}/tmp", 0o1777)
    os.makedirs(f"{root}/opt/runner-cache", exist_ok=True)
    make_runner_tarball(f"{root}/opt/runner-cache/actions-runner-linux-x64-2.330.0.tar.gz")


def union_members(layer_files):
    """Apply layers in order; return {name: TarInfo} of the resulting tree."""
    tree = {}
    for lf in layer_files:
        with tarfile.open(lf, "r:*") as tf:
            for m in tf:
                name = m.name.rstrip("/")
                base = os.path.basename(name)
                if base.startswith(".wh."):
                    raise AssertionError(f"unexpected whiteout {name}")
                tree[name] = m
    return tree


class FixtureCase(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="kata-rootfs-test.")
        cls.rootfs = os.path.join(cls.tmp, "rootfs")
        make_fixture(cls.rootfs)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)


class PlanTests(FixtureCase):
    def test_plan_is_bounded_and_deterministic(self):
        p1 = kri.make_plan(self.rootfs, SMALL_LIMIT)
        p2 = kri.make_plan(self.rootfs, SMALL_LIMIT)
        self.assertEqual(p1.to_json(), p2.to_json())
        self.assertEqual([l.entries for l in p1.layers], [l.entries for l in p2.layers])
        self.assertGreater(len(p1.layers), 1, "fixture must force a multi-layer split")
        for layer in p1.layers:
            self.assertLessEqual(layer.size, SMALL_LIMIT, layer.name)

    def test_excluded_paths_and_kept_entries(self):
        plan = kri.make_plan(self.rootfs, SMALL_LIMIT)
        everything = set(plan.skeleton)
        for layer in plan.layers:
            everything.update(layer.entries)
        for absent in (
            "swap.img", "lost+found", "etc/ssh/ssh_host_ed25519_key", "etc/machine-id",
            "etc/resolv.conf", "home/ubuntu/.bash_history", "home/ubuntu/.ssh",
            "home/ubuntu/.ssh/authorized_keys", "root/.bash_history",
            "usr/lib/modules/6.8.0-oracle", "usr/src/linux-headers-6.8",
            "var/cache/apt/pkgcache.bin", "var/lib/apt/lists/archive_Release",
            "var/lib/cloud/instances", "var/log/cloud-init.log",
            "opt/runner-cache/actions-runner-linux-x64-2.330.0.tar.gz",
            "etc/passwd", "etc/group", "etc/shadow", "etc/gshadow",
        ):
            self.assertNotIn(absent, everything, absent)
        for present in (
            "usr/lib/modules", "usr/src", "var/cache/apt", "var/lib/apt/lists", "var/log",
            "proc", "sys", "dev", "tmp", "opt/runner-cache",
            "etc/ssh/sshd_config", "etc/environment", "usr/bin/dockerd", "usr/bin/fuse-overlayfs",
            "usr/bin/tool with space", "usr/bin/weird\nname", "bin", "lib", "sbin",
            "opt/hostedtoolcache/Python/3.12.3/x64/bin/python3", "usr/share/dotnet/sdk/1/lib.dll",
            "home/ubuntu/.profile", "var/lib/dpkg/status", "etc/sudoers.d/90-cloud-init-users",
        ):
            self.assertIn(present, everything, present)
        self.assertIn("usr/lib/modules", plan.emptied)
        self.assertIn("swap.img", plan.dropped)

    def test_skeleton_covers_every_ancestor(self):
        plan = kri.make_plan(self.rootfs, SMALL_LIMIT)
        skeleton = set(plan.skeleton)
        for layer in plan.layers:
            first = layer.entries[0]
            for anc in kri.ancestors(first):
                self.assertIn(anc, skeleton, f"{anc} missing for unit {first}")

    def test_oversized_regular_file_is_a_hard_error(self):
        with tempfile.TemporaryDirectory() as d:
            make_fixture(d)
            write(f"{d}/opt/huge.bin", b"\x00" * (SMALL_LIMIT + 1))
            with self.assertRaises(kri.PlanError) as cm:
                kri.make_plan(d, SMALL_LIMIT)
            self.assertIn("opt/huge.bin", str(cm.exception))

    def test_whiteout_lookalike_is_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            make_fixture(d)
            write(f"{d}/opt/.wh.evil", b"x")
            with self.assertRaises(kri.PlanError):
                kri.make_plan(d, SMALL_LIMIT)

    def test_no_split_when_limit_is_large(self):
        plan = kri.make_plan(self.rootfs, kri.DEFAULT_LAYER_LIMIT)
        self.assertEqual(len(plan.layers), 1)
        self.assertEqual(plan.skeleton, [])


class AccountsAndEnvTests(FixtureCase):
    def test_accounts_add_runner_and_lock_ubuntu(self):
        acc = kri.build_accounts(self.rootfs)
        self.assertIn("runner:x:1001:1001:GitHub Actions runner:/home/runner:/bin/bash\n", acc["etc/passwd"])
        self.assertIn("runner:x:1001:\n", acc["etc/group"])
        self.assertIn("docker:x:988:ubuntu,runner\n", acc["etc/group"])
        self.assertIn("sudo:x:27:ubuntu,runner\n", acc["etc/group"])
        self.assertIn("adm:x:4:syslog,ubuntu,runner\n", acc["etc/group"])
        self.assertIn("ubuntu:!$6$abc$hash:", acc["etc/shadow"])
        self.assertRegex(acc["etc/shadow"], r"\nrunner:!:\d+:0:99999:7:::\n")
        self.assertIn("docker:!::ubuntu,runner\n", acc["etc/gshadow"])
        self.assertIn("runner:165536:65536\n", acc["etc/subuid"])

    def test_accounts_refuse_existing_runner(self):
        with tempfile.TemporaryDirectory() as d:
            make_fixture(d)
            with open(f"{d}/etc/passwd", "a") as f:
                f.write("runner:x:1001:1001::/home/runner:/bin/bash\n")
            with self.assertRaises(kri.PlanError):
                kri.build_accounts(d)

    def test_accounts_require_docker_group(self):
        with tempfile.TemporaryDirectory() as d:
            make_fixture(d)
            write(f"{d}/etc/group", "root:x:0:\nsudo:x:27:\nadm:x:4:\n")
            with self.assertRaises(kri.PlanError) as cm:
                kri.build_accounts(d)
            self.assertIn("docker", str(cm.exception))

    def test_image_env(self):
        env = dict(e.split("=", 1) for e in kri.image_env(self.rootfs))
        self.assertEqual(env["ImageOS"], "ubuntu24")
        self.assertEqual(env["ImageVersion"], "20260901.1.0")
        self.assertEqual(env["HOME"], "/home/runner")
        self.assertEqual(env["USER"], "runner")
        self.assertEqual(env["XDG_CONFIG_HOME"], "/home/runner/.config")
        self.assertEqual(env["RUNNER_TOOL_CACHE"], "/opt/hostedtoolcache")
        self.assertEqual(env["LANG"], "C.UTF-8")
        self.assertEqual(env["JAVA_TOOL_OPTIONS"], "-Da=1,-Db=2")
        self.assertTrue(env["PATH"].startswith("/usr/local/sbin"))
        self.assertNotIn("ACCEPT_EULA", env)

    def test_image_env_requires_runner_images_markers(self):
        with tempfile.TemporaryDirectory() as d:
            make_fixture(d)
            write(f"{d}/etc/environment", "PATH=/usr/bin\n")
            with self.assertRaises(kri.PlanError):
                kri.image_env(d)

    def test_verify_rootfs_requires_baked_fuse_overlayfs(self):
        with tempfile.TemporaryDirectory() as d:
            make_fixture(d)
            os.remove(f"{d}/usr/bin/fuse-overlayfs")
            with self.assertRaises(kri.PlanError) as cm:
                kri.verify_rootfs(d)
            self.assertIn("fuse-overlayfs", str(cm.exception))


class BuildTests(FixtureCase):
    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.work = os.path.join(cls.tmp, "work")
        env = dict(os.environ, SOURCE_DATE_EPOCH="1700000000")
        cmd = [sys.executable, os.path.join(os.path.dirname(HERE), "kata_rootfs_image.py"), "build",
               "--rootfs", cls.rootfs, "--work", cls.work, "--layer-limit", str(SMALL_LIMIT),
               "--release-tag", "ubuntu24/20260901.1", "--source-revision", "abc123", "--no-compress"]
        subprocess.run(cmd, check=True, env=env)
        with open(os.path.join(cls.work, "image-config.json")) as f:
            cls.config = json.load(f)
        cls.tree = union_members(cls.config["layers"])

    def test_layer_files_and_bounds(self):
        layers = self.config["layers"]
        self.assertTrue(os.path.basename(layers[0]).startswith("layer-000"))
        self.assertTrue(layers[-1].endswith("layer-adapt.tar"))
        for lf in layers[1:-1]:
            self.assertLessEqual(os.path.getsize(lf), SMALL_LIMIT, lf)
        self.assertGreater(len(layers), 3)

    def test_union_tree_content(self):
        t = self.tree
        for absent in ("swap.img", "etc/ssh/ssh_host_ed25519_key", "home/ubuntu/.bash_history",
                       "usr/lib/modules/6.8.0-oracle", "var/lib/cloud/instances",
                       "opt/runner-cache/actions-runner-linux-x64-2.330.0.tar.gz"):
            self.assertNotIn(absent, t, absent)
        self.assertTrue(t["etc/resolv.conf"].isfile())
        self.assertEqual(t["etc/machine-id"].size, 0)
        self.assertTrue(t["bin"].issym())
        self.assertEqual(t["bin"].linkname, "usr/bin")
        self.assertIn("usr/bin/tool with space", t)
        self.assertIn("usr/bin/weird\nname", t)
        self.assertIn("usr/lib/modules", t)
        self.assertTrue(t["usr/lib/modules"].isdir())
        self.assertEqual(t["opt/hostedtoolcache"].mode & 0o7777, 0o777)
        self.assertEqual(t["opt/hostedtoolcache/Python/3.12.3"].mode & 0o7777, 0o777)
        self.assertEqual(t["tmp"].mode & 0o7777, 0o1777)
        self.assertEqual(t["usr/bin/sudo"].mode & 0o7777, 0o4755)
        self.assertIn("var/lib/apt/lists/partial", t)
        self.assertIn("var/cache/apt/archives/partial", t)

    def test_adaptation_layer(self):
        t = self.tree
        for name in ("home/runner/run.sh", "home/runner/bin/Runner.Listener", "home/runner/.bashrc",
                     "home/runner/.config/configstore/.keep", "home/runner/externals/node20/bin/node"):
            self.assertIn(name, t, name)
            self.assertEqual((t[name].uid, t[name].gid), (1001, 1001), name)
        self.assertEqual(t["home/runner"].uid, 1001)
        self.assertEqual(t["home/runner/run.sh"].mode & 0o777, 0o755)
        self.assertEqual(t["etc/sudoers.d/runner"].mode & 0o777, 0o440)
        self.assertEqual(t["etc/shadow"].gid, 42)
        with tarfile.open(self.config["layers"][-1]) as tf:
            def read_member(name):
                member = tf.extractfile(name)
                assert member is not None, name
                return member.read()

            passwd = read_member("etc/passwd").decode()
            sudoers = read_member("etc/sudoers.d/runner").decode()
            release = json.loads(read_member(kri.RELEASE_FILE))
        self.assertIn("runner:x:1001:1001", passwd)
        self.assertEqual(sudoers, "runner ALL=(ALL) NOPASSWD:ALL\n")
        self.assertEqual(release["runner_images_release"], "ubuntu24/20260901.1")
        self.assertEqual(release["runner_version"], "2.330.0")
        self.assertEqual(release["source_revision"], "abc123")
        self.assertEqual(release["image_version"], "20260901.1.0")
        self.assertEqual(release["built_at"], "2023-11-14T22:13:20Z")

    def test_image_config(self):
        c = self.config
        self.assertEqual(c["user"], "1001")
        self.assertEqual(c["workdir"], "/home/runner")
        self.assertIn("ImageOS=ubuntu24", c["env"])
        self.assertEqual(c["labels"]["org.opencontainers.image.version"], "ubuntu24/20260901.1")
        self.assertEqual(c["labels"]["org.opencontainers.image.revision"], "abc123")
        self.assertEqual(c["labels"]["io.cncf.gha-kata-runner.runner-version"], "2.330.0")
        for v in c["labels"].values():
            self.assertNotIn(",", v, "crane -l splits on commas")

    def test_plan_json_written(self):
        with open(os.path.join(self.work, "plan.json")) as f:
            plan = json.load(f)
        self.assertEqual(plan["layer_limit"], SMALL_LIMIT)
        self.assertGreater(plan["skeleton_entries"], 0)


if __name__ == "__main__":
    unittest.main()
