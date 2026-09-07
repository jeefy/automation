# Kata Container Runners (pilot, oke-gha-phx)

Pilot of a **Kata-compatible containerized runner with a VM security
boundary**: every job is an ordinary ARC ephemeral pod whose containers run
inside a Kata Containers microVM (`kata-qemu-runtime-rs`) on a bare-metal OKE
node. The software inside is the actions/runner-images build the `oracle-*`
VM runners boot - taken from the VM build's rootfs, not from a booted VM.

The existing VM runners (`vm-runners/`, `ci/cloudrunners/oci`, the Packer
builders) are untouched and remain the production path.

## Architecture

```
GitHub job (runs-on: oracle-kata-{2cpu-8gb,8cpu-32gb}-x86-64)
  -> ARC listener scales the AutoscalingRunnerSet
  -> ephemeral pod, runtimeClassName kata-qemu-runtime-rs
       QEMU guest on a BM.Standard.E4.128 node (pool: <cluster>-kata-pool1)
       initContainers: scratch perms, runner externals, dind (native sidecar)
       container:      runner (/home/runner/run.sh, uid 1001)
  -> job ends, runner exits, kubelet stops the dind sidecar, pod completes
```

| Piece | Where |
|---|---|
| Bare-metal pool, taint `cncf.io/kata-runner`, autoscaler bounds | `ci/iac/oracle/cluster/kata-nodepool.tf`, `addons.tf`, `tfvars/oke-cncf-gha-phx.tfvars` |
| kata-deploy 4.1.0 (runtime + RuntimeClass) | `apps/kata-deploy.yaml`, `manifests/kata-deploy-values.yaml` |
| Runner scale sets (rendered) | `cncf-kata-2-8-x86.yaml`, `cncf-kata-8-32-x86.yaml` via `render.py` + `IMAGE_DIGEST` |
| Image pre-pull DaemonSet (production digest) | `../kata-support/` (ArgoCD `apps/kata-support.yaml`) |
| Canary namespace: static runner SA + controller Role, manager identity, GitHub secret | `../kata-canary/` (ArgoCD `apps/kata-canary.yaml`) |
| Image build | `.github/scripts/build-kata-runner-image.sh` + `kata_rootfs_image.py` (nightly / manual `periodic-build-new-actions-vm.yml`) |
| Canary + promotion | `.github/workflows/promote-kata-image.yml`, `.github/scripts/kata-canary.sh` |
| Smoke test | `.github/actions/kata-runner-smoke`, `.github/workflows/test-kata-runner.yaml` |

## Runner sizes - read the numbers honestly

Kubernetes `cpu` = one hardware thread. The legacy `oracle-8cpu-32gb-x86-64`
VM is 8 **OCPUs** = 16 threads. The kata sizes below are stated in threads:

| Label | Job budget (runner + dind) | minRunners | Role |
|---|---|---|---|
| `oracle-kata-2cpu-8gb-x86-64` | 2 threads / 8 GiB / 40 GiB scratch | 1 | small common warm reserve |
| `oracle-kata-8cpu-32gb-x86-64` | 8 threads / 32 GiB / 100 GiB scratch | 0 | larger jobs, cold start accepted |

So `oracle-kata-8cpu-32gb` has **half the compute** of `oracle-8cpu-32gb`.
Do not advertise them as equivalent; measure before renaming.

Every container has `requests == limits` (Guaranteed QoS), so the pod cgroup,
the kata guest sizing (sum of container limits) and the scheduler agree on one
bounded budget including dind. The split (runner 750m/3Gi + dind 1250m/5Gi;
3/10Gi + 5/22Gi) favours dind because docker builds, kind and service
containers run there; it is a pilot knob in `render.py` `SIZES`. Scratch is one
`emptyDir` (`sizeLimit`) shared by `_work`, caches, `/tmp` and docker's
data-root, requested as `ephemeral-storage` so the scheduler accounts for it.
The RuntimeClass adds `overhead.podFixed` 250m / 320Mi per pod (kata-deploy
4.1.0 default for `qemu-runtime-rs`).

Capacity: one BM.Standard.E4.128 (128 OCPU / 256 threads, 2 TiB) fits ~30
large or ~110 small pods by CPU; the practical limit is the 1 TiB boot volume
(image cache + scratch: ~9 large + a few small pods at full scratch) - see
"Measurements".

## Warm capacity policy

- ARC `minRunners: 1` on the 2cpu set keeps exactly one small runner idle;
  everything else scales from zero.
- Terraform `kata_autoscaler_min = 1` keeps one bare-metal host up (the warm
  reserve needs a node). **This floor is expensive**: one E4.128 is billed
  24x7. `kata_autoscaler_max = 2` bounds the pilot.
- `min = 0` is possible but untested: a cold node must run kata-deploy
  (installs runtime, restarts containerd) and pre-pull a multi-GB image before
  the first pod starts, and the RuntimeClass `nodeSelector
  katacontainers.io/kata-runtime=true` plus the pod's `cncf.io/kata-runner`
  taint can keep the Cluster Autoscaler from seeing a node group as a fit for
  the pending pod. Do not lower the floor until the "cold bootstrap" live test
  passes.
- Terraform ignores `node_config_details.size` drift after creation so a
  plan after autoscaling does not resize the pool.

## Image: exact VM-image software, container packaging

`ghcr.io/cncf/gha-kata-runner` is produced after the Packer build from
`image.raw` (`build-kata-runner-image.sh`):

- the root partition is loop-mounted **read-only**; nothing is written to
  the source disk; `kata_rootfs_image.py` plans and packs explicit
  NUL-separated file lists (whitespace/newline paths safe, metadata, xattrs
  and sticky bits preserved, `--numeric-owner`);
- layers are bounded (`LAYER_LIMIT_BYTES`, default 9 GiB; GHCR rejects
  >10 GiB) by first-fit-decreasing packing; a single file over the limit is a
  hard error; layer 0 carries the directory skeleton;
- dropped: host kernel modules/headers, `/boot`, apt caches, cloud-init state
  and logs, swap, SSH host keys, `machine-id`, shell histories, `~/.ssh`,
  `/opt/runner-cache`; the `ubuntu` account is password-locked;
- kept: `/opt/hostedtoolcache`, compilers, docker, everything else;
- adaptation layer: `runner` (uid/gid 1001, groups docker/sudo/adm,
  passwordless sudo like the VM's `ubuntu`), `/home/runner` with the cached
  actions-runner tarball unpacked, apt/`/tmp` dirs recreated,
  `/etc/cncf-kata-runner-release` (provenance: runner-images tag, git SHA,
  runner version, build time);
- image config: `/etc/environment` (ImageOS, ImageVersion, tool cache paths,
  ...) baked as env, `USER 1001`, `WORKDIR /home/runner`, OCI labels;
- `fuse-overlayfs` is installed by the Packer build (`ci/gha-runner-vm/main.go`),
  never at pod start; the conversion refuses a rootfs without it.

Limitations of the compatible environment (not a booted VM): no systemd, no
`/etc/environment` processing (env comes from the image config), no host
kernel modules (`modprobe` fails; the kata guest kernel is what runs), docker
uses `fuse-overlayfs` (virtio-fs volumes cannot back overlay2) so image
build/extract is slower than the VM's overlay2, `/proc/cpuinfo` shows the guest
vCPUs, and hardware passthrough/GPU is out of scope.

## Release flow: exact digest, canary, reviewable promotion

1. `periodic-build-new-actions-vm.yml` (`build-x86`, nightly or `workflow_dispatch`)
   pushes `rc-<tag>-amd64` and dispatches `promote-kata-image.yml` with
   `candidate_digest`, `release_tag`, `source_revision`. Every kata-only step
   (GHCR login, crane install, conversion, dispatch) is `continue-on-error`,
   so a kata failure never blocks the VM image release.
   Retrying the conversion when the OCI image already exists means a full
   rebuild (the VM export is not re-read):
   `gh workflow run periodic-build-new-actions-vm.yml -f force_rebuild_x86=true`
   (sets `image_exists=false` for x86 and `GITHUB_PERIODIC=false` so the
   builder does not return early). Retrying only the canary/promotion for an
   already pushed candidate: dispatch `promote-kata-image.yml` directly.
2. `promote-kata-image.yml` (re-dispatch by hand with the same inputs to retry
   a candidate without rebuilding; inputs are validated and never interpolated
   into shell):
   - `preflight`: input validation and an **anonymous pull check** of the exact
     digest against ghcr.io. 401/403 means "private *or* absent" - GHCR does
     not distinguish - so the job fails closed with that wording.
   - `canary-manage` (110 min budget = pre-pull + listener + 25 min pickup +
     test): scoped `KATA_CANARY_KUBECONFIG`; `kata-canary.sh create` renders
     (`render.py --canary-manifest`) and `kubectl create`s exactly two per-run
     objects in namespace `kata-canary`: a pre-pull DaemonSet and an
     AutoscalingRunnerSet `kata-canary-<run>-<attempt>` with the same pod spec
     as the 8cpu production set, using the **static** `kata-canary-runner`
     ServiceAccount and the pre-installed controller Role. On any failure it
     diagnoses, deletes both objects and **cancels its own run** so the queued
     test job cannot wait forever; an `always()` step deletes again. `delete`
     is bounded (300 s per step), waits for the runner pods of the label to be
     gone and fails loudly if the controller leaves anything behind.
   - `canary-test` runs on that label with a real `jobs.<id>.services` nginx
     container (the smoke action probes it), checks `/etc/cncf-kata-runner-release`
     against the *inputs* (`release_tag`, `source_revision` - not the
     workflow's own SHA, so retries from another revision stay honest), then
     runs `kata-runner-smoke`.
   - `promote` runs only when both canary jobs succeeded: tags the digest
     `<tag>-amd64` (informational; nothing consumes tags) and opens a PR that
     writes `IMAGE_DIGEST` and re-renders. Merging the PR is the production
     gate.

`IMAGE_DIGEST` is `unpromoted` until then: `render.py` emits **only RBAC** for
the production sets (no AutoscalingRunnerSet, no pre-pull DaemonSet), so
nothing schedules and no fabricated digest exists anywhere. An all-zero digest
is rejected by `render.py`.

### Canary namespace and identity

`manifests/kata-canary/` (ArgoCD `apps/kata-canary.yaml`) is the blast radius
of the canary: Namespace, token-less runner SA, the controller manager
Role/RoleBinding (same rules gha-runner-scale-set 0.14.1 renders per set), an
ExternalSecret copying `github-arc-secret` from the same OCI vault entry, and
the `kata-canary-manager` identity whose Role allows only
get/list/watch/create/delete/patch on AutoscalingRunnerSets and DaemonSets plus
read on pods/logs/events and ARC sub-resources. It cannot read Secrets, create
RBAC, or see `arc-systems`, so it cannot affect production scale sets.
`test_kata_canary_rbac.py` checks every `kubectl` verb the script issues
against that Role.

This requires the ARC controller to watch all namespaces
(`gha-runner-scale-set-controller-values.yaml`: `watchSingleNamespace: ""`;
the chart then installs cluster-scoped RBAC for the controller). Production
scale sets stay in `arc-systems`.

### Operator prerequisites (one time)

1. The GHCR package `cncf/gha-kata-runner` must be **public** (kata nodes pull
   anonymously; the pre-pull DaemonSets carry no pull secret). Set visibility
   in the package settings after the first `rc-` push.
2. Sync `apps/kata-canary.yaml` and confirm external-secrets created
   `kata-canary/github-arc-secret`.
3. Mint the canary kubeconfig and store it as `KATA_CANARY_KUBECONFIG`:

```sh
export KUBECONFIG=<oke-gha-phx admin kubeconfig>   # never assume a default context
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
TOKEN=$(kubectl -n kata-canary create token kata-canary-manager --duration=2160h)
cat > canary.kubeconfig <<KC
apiVersion: v1
kind: Config
clusters: [{name: oke-gha-phx, cluster: {server: ${SERVER}, certificate-authority-data: ${CA}}}]
users: [{name: kata-canary-manager, user: {token: ${TOKEN}}}]
contexts: [{name: canary, context: {cluster: oke-gha-phx, user: kata-canary-manager, namespace: kata-canary}}]
current-context: canary
KC
gh secret set KATA_CANARY_KUBECONFIG < <(base64 -w0 canary.kubeconfig); shred -u canary.kubeconfig
```

## Security

- The hypervisor boundary is the isolation: `privileged: true` on dind is
  confined to the guest kernel. No `hostPath` volumes anywhere (the
  in-cluster DinD runners mount `/lib/modules` and `/sys/fs/cgroup`; kata
  runners must not).
- `automountServiceAccountToken: false` on runner pods and their SA;
  `enableServiceLinks: false`. The smoke test fails if a token or
  `KUBERNETES_SERVICE_HOST` is visible to a job.
- Instance metadata (169.254.169.254) is reachable from pods on OCI VCN-native
  CNI unless blocked. The kata nodes use instance-principal auth for the
  autoscaler add-on only; still, a job inside a guest can reach IMDS through
  the pod network. Before opening the runners beyond the pilot, add a
  NetworkPolicy/NSG rule denying 169.254.169.254 from the runner pod subnet
  and verify from a job (`curl -m3 -H 'Authorization: Bearer Oracle'
  http://169.254.169.254/opc/v2/instance/` must fail).
- The GitHub token secret is read only by the ARC controller (in both
  namespaces); the canary identity cannot list Secrets.
- `kata-canary` is labelled Pod Security `privileged` (dind needs it inside
  the guest); production runners already run in `arc-systems` under the same
  conditions.

## Upgrading the kata runtime

`kata-deploy` uses `updateStrategy: OnDelete`, so a chart or image bump
rolls out **only** when an operator deletes the DaemonSet pod on a node -
after cordoning and draining it (`kubectl drain --ignore-daemonsets
--delete-emptydir-data`, waiting for running jobs to finish since runner pods
are `restartPolicy: Never`). The install restarts containerd; never let it
happen under running jobs on all nodes at once.

## Image cache

`kata-image-prepull.yaml` keeps the promoted digest present in containerd on
every kata node (init container pulls, then idles), and the canary job adds a
second DaemonSet for the candidate during the run. This is plain pre-pulling
that survives pod churn; a **recreated node** pulls once on join (minutes for
a multi-GB image, gated by the pre-pull rollout in the canary). Lazy pulling
(nydus/erofs snapshotters) is deliberately not enabled; evaluate it as a
separate benchmark experiment if node churn becomes real.

## Measurements to collect during the pilot

Record with `gha_job_startup_duration_seconds` /
`gha_job_execution_duration_seconds` (listener metrics) and node metrics:

1. queue-to-start latency, warm small runner vs cold large runner;
2. pod start breakdown: image present vs pull; kata guest boot;
3. docker build throughput fuse-overlayfs vs a VM runner (same Dockerfile);
4. `go test`/compile wall time at 2 and 8 threads vs `oracle-8cpu-32gb`;
5. boot-volume usage on the node under N concurrent pods (`df` on the node,
   `kubectl describe node` ephemeral-storage) to size `kata_node_boot_volume_size`;
6. memory: guest RSS vs pod limit, to confirm the 320Mi overhead is enough;
7. autoscaler behaviour: does a pending kata pod trigger a scale-up from 1->2?

## Live tests an operator must run (cannot be done from this repo)

The following were **not** executed here (no cluster/registry/credentials):

1. `terraform plan` for `oke-cncf-gha-phx` (expect: new kata pool, autoscaler
   `nodes` = `"<min>:<max>:<pool1>,1:2:<kata-pool>"`) and apply.
2. After ArgoCD syncs `kata-deploy`: `kubectl get runtimeclass kata-qemu-runtime-rs`,
   node label `katacontainers.io/kata-runtime=true` on the BM node, and a
   trivial `runtimeClassName: kata-qemu-runtime-rs` pod prints a guest kernel.
3. Complete the operator prerequisites; trigger
   `periodic-build-new-actions-vm.yml` (or `promote-kata-image.yml` with an
   existing candidate) and confirm the canary set appears in `kata-canary`,
   the test job runs on `kata-canary-<run>-<attempt>`, the set and DaemonSet
   are gone afterwards, and the promotion PR renders cleanly. Verify the
   failure path once: a bad candidate must leave no objects behind and
   cancel its own run.
4. Merge the PR; run `test-kata-runner.yaml` for both sizes.
5. Cold bootstrap test before ever setting `kata_autoscaler_min = 0`: scale
   the pool to 0, submit a job, verify the autoscaler adds a node, kata-deploy
   installs, pre-pull completes, and the job runs.
6. Kata runtime upgrade drill (cordon/drain/delete pod) on one node.
7. IMDS reachability check from a job (see Security).
8. Runner-image conversion on a real `image.raw`: check the layer count,
   sizes and that `crane config` shows the expected env; compare `docker
   version` inside a kata job with an `oracle-8cpu-32gb` VM job.

## Local verification

```sh
python3 -m unittest discover -s .github/scripts/tests -v           # conversion + manifest tests
shellcheck .github/scripts/build-kata-runner-image.sh .github/scripts/kata-canary.sh
python3 ci/cluster/oke-gha-phx/manifests/kata-runners/render.py --check
(cd ci/gha-runner-vm && go vet ./... && go test ./...)
(cd ci/iac/oracle/cluster && terraform init -backend=false && terraform validate)
```
