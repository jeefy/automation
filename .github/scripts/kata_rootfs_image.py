#!/usr/bin/env python3
"""Convert a mounted GHA runner VM rootfs into OCI image layers for kata runners.

This is the planning/packing brain behind build-kata-runner-image.sh. It never
mounts anything and never writes into the source rootfs: it reads a (read-only)
rootfs directory and produces, under a work directory:

  layers/layer-000.tar[.gz]     directory skeleton (every ancestor dir, metadata only)
  layers/layer-001..N.tar[.gz]  rootfs content, greedy-packed under --layer-limit
  layers/layer-adapt.tar        ARC adaptations (runner user, /home/runner, sanitised
                                passwd/shadow, empty machine-id/resolv.conf, release file)
  image-config.json             env / labels / user / workdir for `crane mutate`
  plan.json                     the layer plan (for humans and tests)

Design rules (see ci/cluster/oke-gha-phx/manifests/kata-runners/README.md):

* every archive entry is listed explicitly (NUL separated, --no-recursion), so
  exclusions are exact, whitespace/newline paths are safe and the plan is
  deterministic (sorted, first-fit-decreasing by size);
* a single regular file larger than the layer limit is a hard error - GHCR
  rejects layers over 10 GiB and there is nothing sensible to do automatically;
* the source rootfs is treated as immutable; all changes live in layer-adapt;
* host-kernel and VM-only state is dropped (see EMPTY_DIRS / DROP_GLOBS) while
  every installed tool (hostedtoolcache, compilers, docker...) is preserved.

Runs unprivileged on any directory (tests use a fixture); in CI it runs under
sudo because the real rootfs is root-owned.
"""

from __future__ import annotations

import argparse
import fnmatch
import io
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import time
from dataclasses import dataclass, field
from typing import Dict, Iterable, List, Optional, Tuple

GIB = 1024 ** 3
DEFAULT_LAYER_LIMIT = 9 * GIB  # GHCR refuses layers >10 GiB; keep headroom.

# Directories whose *entry* is kept (mode/owner preserved) but whose contents are
# dropped. Host kernel bits, virtual filesystems, caches and VM build residue.
EMPTY_DIRS: Tuple[str, ...] = (
    "proc",
    "sys",
    "dev",
    "run",
    "mnt",
    "media",
    "tmp",
    "var/tmp",
    "boot",
    "usr/lib/modules",
    "usr/src",
    "var/cache/apt",
    "var/lib/apt/lists",
    "var/lib/cloud",
    "var/log",
    "var/crash",
    "root/.cache",
    "opt/runner-cache",
)

# Entries dropped entirely (glob on the rootfs-relative path). Secrets and
# machine identity from the Packer VM must not leak into a public image.
DROP_GLOBS: Tuple[str, ...] = (
    "swap.img",
    "swapfile",
    "lost+found",
    "etc/ssh/ssh_host_*",
    "etc/machine-id",
    "var/lib/dbus/machine-id",
    "etc/resolv.conf",
    "root/.ssh",
    "root/.ssh/*",
    "root/.bash_history",
    "home/*/.ssh",
    "home/*/.ssh/*",
    "home/*/.bash_history",
    "home/*/.sudo_as_admin_successful",
    "home/*/.cache",
    "home/*/.cache/*",
    "etc/cloud/cloud-init.disabled",
    "var/lib/systemd/random-seed",
)

# Files the adaptation layer rewrites (so they must not also come from the source).
ADAPT_OWNED: Tuple[str, ...] = (
    "etc/passwd",
    "etc/group",
    "etc/shadow",
    "etc/gshadow",
    "etc/subuid",
    "etc/subgid",
)

# Directories (re)created empty in the adaptation layer so apt/dpkg work in jobs.
ADAPT_MKDIRS: Tuple[Tuple[str, int], ...] = (
    ("var/lib/apt/lists/partial", 0o755),
    ("var/cache/apt/archives/partial", 0o755),
    ("var/log/apt", 0o755),
    ("var/log/journal", 0o755),
    ("var/tmp", 0o1777),
    ("tmp", 0o1777),
)

RUNNER_USER = "runner"
RUNNER_UID = 1001
RUNNER_GID = 1001
RUNNER_HOME = "/home/runner"
RUNNER_GROUPS = ("docker", "sudo", "adm")
REQUIRED_BINARIES = ("usr/bin/dockerd", "usr/bin/fuse-overlayfs", "usr/bin/sudo")
RELEASE_FILE = "etc/cncf-kata-runner-release"

# Only these /etc/environment keys are dropped: they are VM/agent specific.
ENV_SKIP_KEYS = frozenset({"ACCEPT_EULA"})

TAR_BLOCK = 512
TAR_EOF = 2 * TAR_BLOCK
PAX_FIXED_RECORDS = 256  # mtime/atime/ctime/size/uid/gid records written by --format=posix


class PlanError(Exception):
    pass


@dataclass
class Entry:
    rel: str
    size: int  # estimated archive footprint incl. headers
    is_dir: bool


@dataclass
class Layer:
    name: str
    entries: List[str] = field(default_factory=list)
    size: int = 0


@dataclass
class Plan:
    layer_limit: int
    skeleton: List[str]
    layers: List[Layer]
    dropped: List[str]
    emptied: List[str]

    def to_json(self) -> dict:
        return {
            "layer_limit": self.layer_limit,
            "skeleton_entries": len(self.skeleton),
            "layers": [
                {"name": l.name, "entries": len(l.entries), "estimated_bytes": l.size}
                for l in self.layers
            ],
            "dropped": self.dropped,
            "emptied": self.emptied,
        }


def _match_any(rel: str, globs: Iterable[str]) -> bool:
    return any(fnmatch.fnmatchcase(rel, g) for g in globs)


def is_dropped(rel: str) -> bool:
    if _match_any(rel, DROP_GLOBS):
        return True
    return rel in ADAPT_OWNED


def is_emptied_dir(rel: str) -> bool:
    return rel in EMPTY_DIRS


def under_emptied_dir(rel: str) -> bool:
    return any(rel.startswith(d + "/") for d in EMPTY_DIRS)


def check_whiteout_safe(rel: str) -> None:
    for part in rel.split("/"):
        if part.startswith(".wh."):
            raise PlanError(f"path {rel!r} would be interpreted as an OCI whiteout")


def _round_block(n: int) -> int:
    return (n + TAR_BLOCK - 1) // TAR_BLOCK * TAR_BLOCK


def xattr_bytes(abs_path: str) -> int:
    try:
        names = os.listxattr(abs_path, follow_symlinks=False)
    except OSError:
        return TAR_BLOCK
    total = 0
    for n in names:
        try:
            total += len(n) + len(os.getxattr(abs_path, n, follow_symlinks=False)) + 32
        except OSError:
            total += TAR_BLOCK
    return total


def estimate_entry_size(rel: str, abs_path: str, st: os.stat_result) -> int:
    """Upper bound of the entry's footprint in a --format=posix tar stream."""
    data = st.st_size if stat.S_ISREG(st.st_mode) else 0
    pax_data = PAX_FIXED_RECORDS + 2 * len(rel.encode("utf-8", "surrogateescape")) + xattr_bytes(abs_path)
    if stat.S_ISLNK(st.st_mode):
        pax_data += 2 * len(os.readlink(abs_path).encode("utf-8", "surrogateescape"))
    return TAR_BLOCK + _round_block(pax_data) + TAR_BLOCK + _round_block(data)


def walk_rootfs(rootfs: str) -> Tuple[Dict[str, Entry], Dict[str, List[str]], List[str], List[str]]:
    """Walk the rootfs without following symlinks.

    Returns (entries, children, dropped, emptied): entries keyed by rel path;
    children maps a dir rel path to sorted child rel paths (only kept ones).
    Directory sizes are aggregated over kept descendants.
    """
    entries: Dict[str, Entry] = {}
    children: Dict[str, List[str]] = {}
    dropped: List[str] = []
    emptied: List[str] = []

    def visit(rel: str, abs_path: str) -> int:
        st = os.lstat(abs_path)
        if rel:
            check_whiteout_safe(rel)
        if stat.S_ISSOCK(st.st_mode):
            dropped.append(rel)
            return 0
        if rel and is_dropped(rel):
            dropped.append(rel)
            return 0
        size = estimate_entry_size(rel, abs_path, st)
        if not stat.S_ISDIR(st.st_mode):
            entries[rel] = Entry(rel, size, False)
            return size
        kids: List[str] = []
        if rel and is_emptied_dir(rel):
            emptied.append(rel)
        else:
            with os.scandir(abs_path) as it:
                names = sorted(e.name for e in it)
            for name in names:
                crel = f"{rel}/{name}" if rel else name
                csize = visit(crel, os.path.join(abs_path, name))
                if crel in entries:
                    kids.append(crel)
                    size += csize
        entries[rel] = Entry(rel, size, True)
        children[rel] = kids
        return size

    visit("", rootfs)
    return entries, children, dropped, emptied


def split_units(entries: Dict[str, Entry], children: Dict[str, List[str]], limit: int) -> List[str]:
    """Return rel paths of pack units: subtrees (or single entries) <= limit."""
    units: List[str] = []

    def rec(rel: str) -> None:
        e = entries[rel]
        if e.size <= limit or not e.is_dir:
            if e.size > limit:
                raise PlanError(
                    f"{rel!r} is {e.size} bytes which exceeds the layer limit of {limit} bytes; "
                    "drop it (DROP_GLOBS) or raise --layer-limit (GHCR max is 10 GiB)"
                )
            units.append(rel)
            return
        kids = children.get(rel, [])
        if not kids:
            units.append(rel)
            return
        for c in kids:
            rec(c)

    for top in children.get("", []):
        rec(top)
    return units


def subtree(rel: str, entries: Dict[str, Entry], children: Dict[str, List[str]]) -> List[str]:
    out = [rel]
    stack = list(reversed(children.get(rel, [])))
    while stack:
        cur = stack.pop()
        out.append(cur)
        stack.extend(reversed(children.get(cur, [])))
    return out


def ancestors(rel: str) -> List[str]:
    parts = rel.split("/")
    return ["/".join(parts[:i]) for i in range(1, len(parts))]


def pack(units: List[str], entries: Dict[str, Entry], children: Dict[str, List[str]], limit: int) -> Tuple[List[str], List[Layer]]:
    """First-fit-decreasing bin packing of units into layers <= limit."""
    ordered = sorted(units, key=lambda r: (-entries[r].size, r))
    bins: List[Layer] = []
    for rel in ordered:
        size = entries[rel].size
        target = None
        for b in bins:
            if b.size + size <= limit:
                target = b
                break
        if target is None:
            target = Layer(name="", size=TAR_EOF)
            bins.append(target)
        target.entries.append(rel)
        target.size += size
    skeleton = set()
    for rel in units:
        skeleton.update(ancestors(rel))
    layers: List[Layer] = []
    for idx, b in enumerate(bins, start=1):
        full: List[str] = []
        for rel in sorted(b.entries):
            full.extend(subtree(rel, entries, children))
        layers.append(Layer(name=f"layer-{idx:03d}", entries=full, size=b.size))
    return sorted(skeleton), layers


def make_plan(rootfs: str, limit: int = DEFAULT_LAYER_LIMIT) -> Plan:
    if limit <= 0:
        raise PlanError("layer limit must be positive")
    entries, children, dropped, emptied = walk_rootfs(rootfs)
    units = split_units(entries, children, limit - TAR_EOF)
    skeleton, layers = pack(units, entries, children, limit)
    return Plan(layer_limit=limit, skeleton=skeleton, layers=layers, dropped=sorted(dropped), emptied=sorted(emptied))


def tar_base_cmd(tar_bin: str, rootfs: str, out: str, list_file: str) -> List[str]:
    return [
        tar_bin,
        "--create",
        "--file", out,
        "--format=posix",
        "--numeric-owner",
        "--xattrs",
        "--xattrs-include=*",
        "--blocking-factor=1",
        "--warning=no-file-changed",
        "--directory", rootfs,
        "--no-recursion",
        "--null",
        "--files-from", list_file,
    ]


def write_list(path: str, rels: Iterable[str]) -> None:
    with open(path, "wb") as f:
        for r in rels:
            f.write(r.encode("utf-8", "surrogateescape") + b"\0")


def compressor() -> Optional[List[str]]:
    if shutil.which("pigz"):
        return ["pigz", "-n"]
    if shutil.which("gzip"):
        return ["gzip", "-n"]
    return None


def create_layer_tar(tar_bin: str, rootfs: str, layers_dir: str, name: str, rels: List[str], compress: bool) -> str:
    list_file = os.path.join(layers_dir, f"{name}.list")
    write_list(list_file, rels)
    out = os.path.join(layers_dir, f"{name}.tar")
    cmd = tar_base_cmd(tar_bin, rootfs, out, list_file)
    comp = compressor() if compress else None
    if comp:
        out += ".gz"
        cmd[cmd.index("--file") + 1] = out
        cmd.append("--use-compress-program=" + " ".join(comp))
    subprocess.run(cmd, check=True)
    return out


def read_text(rootfs: str, rel: str, default: Optional[str] = None) -> str:
    p = os.path.join(rootfs, rel)
    try:
        with open(p, "r", encoding="utf-8", errors="surrogateescape") as f:
            return f.read()
    except FileNotFoundError:
        if default is None:
            raise
        return default


def parse_colon_db(text: str) -> List[List[str]]:
    rows = []
    for line in text.splitlines():
        if not line.strip() or line.startswith("#"):
            continue
        rows.append(line.split(":"))
    return rows


def render_colon_db(rows: List[List[str]]) -> str:
    return "".join(":".join(r) + "\n" for r in rows)


def build_accounts(rootfs: str, lock_users: Iterable[str] = ("ubuntu",)) -> Dict[str, str]:
    """Return new contents for passwd/group/shadow/gshadow/subuid/subgid."""
    passwd = parse_colon_db(read_text(rootfs, "etc/passwd"))
    group = parse_colon_db(read_text(rootfs, "etc/group"))
    shadow = parse_colon_db(read_text(rootfs, "etc/shadow", ""))
    gshadow = parse_colon_db(read_text(rootfs, "etc/gshadow", ""))

    names = {r[0] for r in passwd}
    uids = {r[2] for r in passwd}
    gnames = {r[0] for r in group}
    gids = {r[2] for r in group}
    if RUNNER_USER in names or str(RUNNER_UID) in uids:
        raise PlanError(f"rootfs already defines user {RUNNER_USER!r}/uid {RUNNER_UID}; refusing to guess")
    if RUNNER_USER in gnames or str(RUNNER_GID) in gids:
        raise PlanError(f"rootfs already defines group {RUNNER_USER!r}/gid {RUNNER_GID}; refusing to guess")
    missing = [g for g in RUNNER_GROUPS if g not in gnames]
    if missing:
        raise PlanError(f"rootfs lacks required groups {missing}; is this a runner-images build with docker installed?")

    passwd.append([RUNNER_USER, "x", str(RUNNER_UID), str(RUNNER_GID), "GitHub Actions runner", RUNNER_HOME, "/bin/bash"])
    group.append([RUNNER_USER, "x", str(RUNNER_GID), ""])
    for row in group:
        if row[0] in RUNNER_GROUPS:
            members = [m for m in row[3].split(",") if m] if len(row) > 3 else []
            if RUNNER_USER not in members:
                members.append(RUNNER_USER)
            while len(row) < 4:
                row.append("")
            row[3] = ",".join(members)

    days = str(int(time.time() // 86400))
    locked = set(lock_users)
    for row in shadow:
        if row[0] in locked and len(row) > 1 and row[1] not in ("!", "*", "!!") and not row[1].startswith("!"):
            row[1] = "!" + row[1]
    shadow.append([RUNNER_USER, "!", days, "0", "99999", "7", "", "", ""])
    gshadow.append([RUNNER_USER, "!", "", ""])
    for row in gshadow:
        if row[0] in RUNNER_GROUPS:
            while len(row) < 4:
                row.append("")
            members = [m for m in row[3].split(",") if m]
            if RUNNER_USER not in members:
                members.append(RUNNER_USER)
            row[3] = ",".join(members)

    out = {
        "etc/passwd": render_colon_db(passwd),
        "etc/group": render_colon_db(group),
        "etc/shadow": render_colon_db(shadow),
        "etc/gshadow": render_colon_db(gshadow),
    }
    for sub in ("etc/subuid", "etc/subgid"):
        text = read_text(rootfs, sub, "")
        rows = parse_colon_db(text)
        nxt = 100000
        for r in rows:
            try:
                nxt = max(nxt, int(r[1]) + int(r[2]))
            except (IndexError, ValueError):
                continue
        rows.append([RUNNER_USER, str(nxt), "65536"])
        out[sub] = render_colon_db(rows)
    return out


def find_runner_tarball(rootfs: str) -> str:
    d = os.path.join(rootfs, "opt/runner-cache")
    cands = sorted(n for n in os.listdir(d) if re.match(r"actions-runner-linux-(x64|arm64)-[0-9.]+\.tar\.gz$", n)) if os.path.isdir(d) else []
    if not cands:
        raise PlanError("no actions-runner tarball under opt/runner-cache; the Packer build must run install-runner-package.sh")
    return os.path.join(d, cands[-1])


def parse_etc_environment(text: str) -> Dict[str, str]:
    env: Dict[str, str] = {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[len("export "):]
        if "=" not in line:
            continue
        k, v = line.split("=", 1)
        k = k.strip()
        v = v.strip()
        if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
            v = v[1:-1]
        if not re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", k):
            continue
        env[k] = v
    return env


def image_env(rootfs: str) -> List[str]:
    """Image config env derived from /etc/environment (+ locale), HOME-expanded."""
    env = parse_etc_environment(read_text(rootfs, "etc/environment", ""))
    for k in ENV_SKIP_KEYS:
        env.pop(k, None)
    locale = parse_etc_environment(read_text(rootfs, "etc/default/locale", ""))
    for k in ("LANG", "LC_ALL", "LANGUAGE"):
        if k in locale and k not in env:
            env[k] = locale[k]
    env.setdefault("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
    env["HOME"] = RUNNER_HOME
    env["USER"] = RUNNER_USER
    env["LOGNAME"] = RUNNER_USER
    env.setdefault("AGENT_TOOLSDIRECTORY", "/opt/hostedtoolcache")
    env.setdefault("RUNNER_TOOL_CACHE", env["AGENT_TOOLSDIRECTORY"])
    for k in ("ImageOS", "ImageVersion"):
        if k not in env:
            raise PlanError(f"/etc/environment lacks {k}; this does not look like an actions/runner-images rootfs")
    out = []
    for k in sorted(env):
        v = env[k].replace("${HOME}", RUNNER_HOME).replace("$HOME", RUNNER_HOME)
        if "\n" in v:
            raise PlanError(f"environment value for {k} contains a newline")
        out.append(f"{k}={v}")
    return out


def _tarinfo(name: str, mode: int, uid: int, gid: int, mtime: int, typ: bytes = tarfile.REGTYPE, size: int = 0) -> tarfile.TarInfo:
    ti = tarfile.TarInfo(name)
    ti.type = typ
    ti.mode = mode
    ti.uid, ti.gid = uid, gid
    ti.uname, ti.gname = "", ""
    ti.mtime = mtime
    ti.size = size
    return ti


def build_adapt_layer(rootfs: str, out_path: str, release: dict, mtime: int) -> dict:
    """Write layer-adapt.tar. Returns a summary dict (runner version etc.)."""
    accounts = build_accounts(rootfs)
    runner_tgz = find_runner_tarball(rootfs)
    version_match = re.search(r"-([0-9.]+)\.tar\.gz$", os.path.basename(runner_tgz))
    if version_match is None:
        raise PlanError(f"cannot identify runner version in {runner_tgz!r}")
    runner_version = version_match.group(1)
    release = dict(release, runner_version=runner_version)
    release_text = json.dumps(release, indent=2, sort_keys=True) + "\n"

    with tarfile.open(out_path, "w", format=tarfile.PAX_FORMAT) as tf:
        def add_bytes(name: str, data: bytes, mode: int, uid: int = 0, gid: int = 0) -> None:
            ti = _tarinfo(name, mode, uid, gid, mtime, size=len(data))
            tf.addfile(ti, io.BytesIO(data))

        def add_dir(name: str, mode: int, uid: int = 0, gid: int = 0) -> None:
            tf.addfile(_tarinfo(name, mode, uid, gid, mtime, typ=tarfile.DIRTYPE))

        for rel, mode in ADAPT_MKDIRS:
            add_dir(rel, mode)
        for rel, text in accounts.items():
            mode = 0o640 if rel in ("etc/shadow", "etc/gshadow") else 0o644
            gid = 42 if rel in ("etc/shadow", "etc/gshadow") else 0  # shadow group on Debian/Ubuntu
            add_bytes(rel, text.encode(), mode, gid=gid)
        add_dir("etc/sudoers.d", 0o750)
        add_bytes("etc/sudoers.d/runner", f"{RUNNER_USER} ALL=(ALL) NOPASSWD:ALL\n".encode(), 0o440)
        add_bytes("etc/machine-id", b"", 0o444)
        add_bytes("etc/resolv.conf", b"", 0o644)  # runtime bind-mounts the real one
        add_bytes(RELEASE_FILE, release_text.encode(), 0o444)

        # /home/runner: skeleton files + the runner package, owned by the runner.
        add_dir("home", 0o755)
        add_dir("home/runner", 0o750, RUNNER_UID, RUNNER_GID)
        skel = os.path.join(rootfs, "etc/skel")
        if os.path.isdir(skel):
            for dirpath, dirnames, filenames in os.walk(skel):
                dirnames.sort()
                rel_dir = os.path.relpath(dirpath, skel)
                for d in dirnames:
                    p = os.path.join(dirpath, d)
                    rel = os.path.normpath(os.path.join("home/runner", rel_dir, d))
                    add_dir(rel, stat.S_IMODE(os.lstat(p).st_mode), RUNNER_UID, RUNNER_GID)
                for f in sorted(filenames):
                    p = os.path.join(dirpath, f)
                    st = os.lstat(p)
                    rel = os.path.normpath(os.path.join("home/runner", rel_dir, f))
                    if stat.S_ISLNK(st.st_mode):
                        ti = _tarinfo(rel, 0o777, RUNNER_UID, RUNNER_GID, mtime, typ=tarfile.SYMTYPE)
                        ti.linkname = os.readlink(p)
                        tf.addfile(ti)
                    elif stat.S_ISREG(st.st_mode):
                        with open(p, "rb") as fh:
                            add_bytes(rel, fh.read(), stat.S_IMODE(st.st_mode), RUNNER_UID, RUNNER_GID)
        with tarfile.open(runner_tgz, "r:gz") as src:
            for m in src:
                name = m.name.lstrip("./")
                if not name:
                    continue
                m2 = m
                m2.name = "home/runner/" + name
                m2.uid, m2.gid = RUNNER_UID, RUNNER_GID
                m2.uname, m2.gname = "", ""
                if m.isfile():
                    tf.addfile(m2, src.extractfile(m))
                else:
                    tf.addfile(m2)
    return {"runner_version": runner_version, "runner_tarball": os.path.basename(runner_tgz)}


def verify_rootfs(rootfs: str) -> None:
    missing = [b for b in REQUIRED_BINARIES if not os.path.exists(os.path.join(rootfs, b))]
    if missing:
        raise PlanError(
            f"rootfs lacks {missing}. fuse-overlayfs and docker must be baked by the Packer build "
            "(ci/gha-runner-vm/main.go), not installed at pod start."
        )
    # /lib -> usr/lib on Ubuntu 24.04; the EMPTY_DIRS policy uses the real path.
    if os.path.isdir(os.path.join(rootfs, "lib/modules")) and not os.path.islink(os.path.join(rootfs, "lib")):
        raise PlanError("rootfs has a real /lib/modules (non-merged /usr); update EMPTY_DIRS")


def cmd_build(a: argparse.Namespace) -> int:
    rootfs = os.path.abspath(a.rootfs)
    work = os.path.abspath(a.work)
    layers_dir = os.path.join(work, "layers")
    os.makedirs(layers_dir, exist_ok=True)
    verify_rootfs(rootfs)

    plan = make_plan(rootfs, a.layer_limit)
    with open(os.path.join(work, "plan.json"), "w") as f:
        json.dump(plan.to_json(), f, indent=2)
    print(f"plan: {len(plan.layers)} content layers, {len(plan.skeleton)} skeleton dirs, "
          f"{len(plan.dropped)} dropped, {len(plan.emptied)} emptied", file=sys.stderr)

    mtime = int(os.environ.get("SOURCE_DATE_EPOCH", "0"))
    env = image_env(rootfs)
    envmap = dict(e.split("=", 1) for e in env)
    release = {
        "image_os": envmap.get("ImageOS"),
        "image_version": envmap.get("ImageVersion"),
        "runner_images_release": a.release_tag,
        "source_repository": a.source_repo,
        "source_revision": a.source_revision,
        "built_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(mtime)),
        "layer_limit_bytes": a.layer_limit,
        "content_layers": len(plan.layers),
        "conversion_tool": "cncf/automation .github/scripts/kata_rootfs_image.py",
        "notes": "kata guest kernel; host kernel modules, VM identity and caches were dropped",
    }

    files: List[str] = []
    if not a.plan_only:
        files.append(create_layer_tar(a.tar, rootfs, layers_dir, "layer-000", plan.skeleton, a.compress))
        for layer in plan.layers:
            files.append(create_layer_tar(a.tar, rootfs, layers_dir, layer.name, layer.entries, a.compress))
        adapt_path = os.path.join(layers_dir, "layer-adapt.tar")
        summary = build_adapt_layer(rootfs, adapt_path, release, mtime)
        files.append(adapt_path)
    else:
        summary = {"runner_version": None}

    labels = {
        "org.opencontainers.image.title": "gha-kata-runner",
        "org.opencontainers.image.description": "actions/runner-images Ubuntu rootfs repacked for kata-containers ARC runners",
        "org.opencontainers.image.source": a.source_repo,
        "org.opencontainers.image.revision": a.source_revision,
        "org.opencontainers.image.version": a.release_tag,
        "org.opencontainers.image.created": release["built_at"],
        "io.cncf.gha-kata-runner.image-version": envmap.get("ImageVersion", ""),
        "io.cncf.gha-kata-runner.runner-version": summary.get("runner_version") or "",
    }
    config = {
        "user": str(RUNNER_UID),
        "workdir": RUNNER_HOME,
        "env": env,
        "labels": {k: v for k, v in labels.items() if v},
        "layers": files,
    }
    with open(os.path.join(work, "image-config.json"), "w") as f:
        json.dump(config, f, indent=2)
    return 0


def main(argv: Optional[List[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    b = sub.add_parser("build", help="plan + write layers + image-config.json")
    b.add_argument("--rootfs", required=True)
    b.add_argument("--work", required=True)
    b.add_argument("--layer-limit", type=int, default=DEFAULT_LAYER_LIMIT)
    b.add_argument("--release-tag", required=True, help="actions/runner-images release tag")
    b.add_argument("--source-repo", default="https://github.com/cncf/automation")
    b.add_argument("--source-revision", default="unknown")
    b.add_argument("--tar", default="tar")
    b.add_argument("--no-compress", dest="compress", action="store_false")
    b.add_argument("--plan-only", action="store_true")
    b.set_defaults(func=cmd_build)
    a = p.parse_args(argv)
    try:
        return a.func(a)
    except (PlanError, subprocess.CalledProcessError, FileNotFoundError) as e:
        print(f"error: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
