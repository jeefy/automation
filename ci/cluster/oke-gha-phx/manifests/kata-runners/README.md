# Kata Container Runners (pilot)

Pilot replacement for the OCI **VM-per-job** runner architecture
(`ci/cloudrunners/oci` + `ci/gha-runner-vm*` Packer images + the
`vm-runners/` AutoscalingRunnerSets). Instead of launching a dedicated OCI
compute instance for every CI job, the runner is a normal ARC ephemeral pod
whose containers execute inside a [Kata Containers](https://katacontainers.io/)
microVM. This keeps VM-grade workload isolation (separate guest kernel,
hypervisor boundary) while removing the OCI instance-launch path, the nightly
Packer image pipeline, spot preemption handling, and the VM cleanup CronJobs.

## Architecture

```
GitHub job (runs-on: oracle-kata-8cpu-32gb-x86-64)
  -> ARC listener scales AutoscalingRunnerSet
  -> ephemeral runner pod (runtimeClassName: kata-qemu-runtime-rs)
       microVM boots on a bare-metal OKE node (~1-3s)
       runner container + privileged dind sidecar run inside the guest
  -> job completes, pod (and microVM) deleted
```

## Moving pieces

| Piece | Where |
|---|---|
| Bare-metal OKE node pool (taint `cncf.io/kata-runner`) | `ci/iac/oracle/cluster/kata-nodepool.tf`, enabled per-cluster via tfvars |
| kata-deploy (installs runtime + `kata-qemu-runtime-rs` RuntimeClass) | ArgoCD app `apps/kata-deploy.yaml`, values in `manifests/kata-deploy-values.yaml` |
| Runner scale sets | this directory, synced by `apps/kata-runners.yaml` |
| Runner image (`ghcr.io/cncf/gha-kata-runner`) | built in `periodic-build-new-actions-vm.yml` from the same Packer rootfs as the VM image |
| Smoke test | `.github/workflows/test-kata-runner.yaml` |

## Runner image: exact VM-image parity

`ghcr.io/cncf/gha-kata-runner` is not a separate Dockerfile build. The
nightly `periodic-build-new-actions-vm.yml` workflow already produces
`image.raw` via Packer from upstream `actions/runner-images`; after the
Packer build, `.github/scripts/build-kata-runner-image.sh` mounts that same
rootfs and repacks it as a container image, so `oracle-kata-*` jobs see the
identical toolset (`/opt/hostedtoolcache`, compilers, docker version, ...) as
`oracle-*` VM jobs. Build-time adaptations:

- a `runner` user (uid 1001) is created and the cached actions-runner
  tarball is unpacked to `/home/runner` (ARC pod contract; the VM instead
  starts the runner over SSH as `ubuntu`)
- `ImageOS`/`ImageVersion`/toolcache env vars are baked into the image
  config, since nothing reads `/etc/environment` in a container
- the host kernel, `/lib/modules`, and apt caches are dropped (the kata
  guest boots its own kernel)
- the rootfs is split into <=9GB layers (GHCR rejects layers over 10GB)

The image follows the same `rc-` release-candidate flow as the OCI custom
images: pushed as `rc-<tag>-amd64`, promoted to `<tag>-amd64` +
`latest-amd64` by the `release-kata-image` job after the runner tests pass.
arm64 is out of scope for the pilot (the arm build uses the native
`oracle-oci` Packer builder and produces no local rootfs to convert).

## Image distribution

The image is large (tens of GB), which shapes distribution:

- containerd caches it per node; with a small static BM pool the pull cost
  is paid once per node per nightly release, not per job
- if node churn grows (autoscaled BM pool), add a pre-pull DaemonSet or
  evaluate lazy-pulling via the nydus/erofs snapshotter, which kata-deploy
  supports per-shim (`shims.qemu-runtime-rs.containerd.snapshotter`)
- workload images pulled by jobs inside DinD start cold per microVM; a
  cluster-local pull-through registry cache plus `--registry-mirror` on the
  dind `dockerd` is the highest-leverage follow-up optimization

## Why bare metal

OCI VM shapes do not expose nested virtualization, and kata boots a QEMU
guest per pod. The kata node pool therefore must use a `BM.Standard.*`
shape. Runner pods bin-pack onto the bare-metal hosts via normal resource
requests instead of one instance per job.

## Kata-specific adaptations vs. the in-cluster DinD runners

Compared to `ci/cluster/oci/runners/*/install.yaml`:

- `runtimeClassName: kata-qemu-runtime-rs` on the runner pod template.
- runner and dind containers use the same `gha-kata-runner` image, so the
  in-VM docker daemon version matches the VM runners exactly.
- dockerd runs with `--storage-driver=fuse-overlayfs`: emptyDir volumes are
  exposed to the guest via virtio-fs, which cannot serve as an overlayfs
  upper layer, so overlay2 fails inside kata.
- No `/lib/modules` or `/sys/fs/cgroup` hostPath mounts: the guest runs its
  own kernel; host paths are meaningless (and harmful) inside the microVM.
- `privileged: true` on dind is confined to the guest kernel by the
  hypervisor boundary - it does not grant host access.

## Rollout status

Coexistence phase: `vm-runners/` remain the production path and keep their
`oracle-*` labels. Kata runners use distinct `oracle-kata-*` labels. Once
capacity and reliability are proven, vm-runner scale sets, the
`ci/cloudrunners/oci` launcher, the Packer image builders, and the
VM-housekeeping CronJobs can be removed.

## Known limitations

- microVM cold start adds ~1-3s per job.
- Guest memory overhead (~150-350MiB per pod) on top of pod requests.
- Jobs relying on host-kernel modules or hardware passthrough will behave
  differently under the guest kernel.
- GPU runners are out of scope for this pilot.
