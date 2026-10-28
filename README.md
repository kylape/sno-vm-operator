# SNO VM operator

Provision preinstalled Single Node OpenShift VMs on OpenShift Virtualization.
Users will request SNO environments through the LLM Workload Environment
operator. This repository owns the SNO runtime controller and its Tekton build;
[image preparation and the approved design](https://gitlab.cee.redhat.com/klape/inference-engineering-scratchpad/-/tree/main/sno-virt-image)
are maintained separately.

## Implementation status

The deployed implementation is the **per-node seed cache controller** and
operator image build pipeline, with no seed images enabled. Source now includes
the immutable `SNOVirtualMachine` API/CRD, durable identity/address allocations,
local disk/VM/API resource builders, and independent bootstrap credentials.
These are tested provisioning components; the complete instance reconciler,
readiness handoff, and storage finalizer are not yet wired into the manager.
`instances.enabled: true` is rejected explicitly in this build.

The disposable reference SNO is installed and healthy, with verified external
API TLS/readiness and an internally reachable application route. The
[LWE SNO adapter](https://github.com/kylape/llm-workload-environment-operator/pull/1)
is implemented in a draft PR. Seed capture, two-clone recert identity isolation,
per-node cache qualification, and complete provider integration remain to be
validated. No five-minute startup result is claimed.

The host currently uses TopoLVM without snapshot classes; CDI selects `copy`.
A cached seed avoids registry downloads but still requires a measured clone
benchmark. Do not alter existing LVM device classes to enable thin provisioning
without a separate storage decision.

## Develop

Go 1.26 is required. Run `make test` and `make build`. The controller starts with
`bin/manager --config=config/cache.example.yaml` and an ordinary kubeconfig or
in-cluster service account. Running the controller creates Kubernetes resources;
use an approved target cluster. The image is Linux amd64 initially.

The cache controller polls every 15 seconds under leader election. Restart after
editing policy; configuration is loaded at startup. It does not expose a
standalone user provisioning CLI.

## Build with Tekton

The operator image repository is public: `quay.io/klape/sno-vm-operator`.
The SNO disk repository is private: `quay.io/klape/sno-vm-image`.

1. Install `config/tekton/platform.yaml` in the target cluster. The isolated
   `sno-vm-build` namespace and builder service account are for builds only.
   Buildah requires the included permission to use the privileged SCC.
2. Create a `quay-push` Secret in that namespace with a `.dockerconfigjson` key
   containing credentials scoped to operator image publication. Keep it outside
   Git. This is distinct from the CDI read-only registry Secret below.
3. Install `config/tekton/pipeline.yaml` in `sno-vm-build`.
4. Copy `config/tekton/pipelinerun.example.yaml` outside the repository. Set
   `git-revision` to the full pushed commit SHA and choose a corresponding image
   tag. Submit it as a new PipelineRun.
5. Record the `source-commit`, `image-digest`, and `image-reference` results.
   Deploy the returned digest, rather than an evolving tag.

The pipeline accepts only this repository's HTTPS URL and a full commit SHA.
Checkout and Go tests do not receive the registry-auth workspace. Tests run
before Buildah receives credentials. Only trusted source should be built with
push credentials. The build workspace is a 5 GiB worker LVM PVC; delete completed
PipelineRuns and their workspace PVCs according to the platform retention policy.
This initial checkout expects the GitHub repository to be publicly readable;
private source authentication is not configured.

## Configure seed caching

`config/cache.example.yaml` enables no images by default. Before enabling an
image, prove that its powered-off disk is safe to clone and record its immutable
Quay digest. Copy the policy and supply the digest, `500Gi` disk size, and the
read-only registry Secret name. Use one placement per worker storage pool.
Nodes matching multiple placements are rejected. Only amd64 nodes matching the
configured worker selectors participate. Cordoned, unready, or untolerated nodes
remain pending in coverage.

For CDI `pullMethod: pod`, `seed-registry-read` is an Opaque Secret in
`sno-vm-system` with `accessKeyId` (Quay robot username) and `secretKey` (robot
password). The operator only checks that these keys exist. CDI uses them for the
import. RBAC permits reads of that Secret name only; adjust it if changing policy.

Render deployment resources with `kubectl kustomize config` and review before
applying. The manifest pins the initial successful Tekton digest; replace it
with the result of subsequent builds when deploying new controller code.
The runtime operator uses restricted permissions; it does not require the build
service account's privileged SCC. Cache source PVCs are raw block volumes.

A small scheduler-bound consumer Pod selects the intended node for each WFFC
cache volume. It never reads the disk and is removed once volume binding is
complete. CDI imports the immutable registry image. A source counts as ready only
when CDI reports success and the PVC/PV binding confirms the intended local node.
Actual VM clone usability and throughput still require the feasibility proof.

## Observe and recover

The `sno-seed-cache-status` ConfigMap in the cache namespace contains
`status.json`, with per-image eligible/ready/pending/failed counts and each
node's source PVC, import attempts, phase, and diagnostic message. Its retry
ledger is durable across controller restarts. A failed import is retried after
at least one minute up to `maxImportAttempts`. Imports that remain pending are
reported; they are not automatically destroyed on a timeout. Overall imports,
including retained in-progress revisions, obey `maxConcurrentImports`.

Before resetting exhausted attempts, inspect CDI events, credentials, scheduling,
and storage. Then edit only the relevant entry's `attempts` in the status JSON.
Deleting a lost completed cache's stale DataVolume starts a new repair cycle;
ordinary namespace/instance deletion must not remove shared caches.

Removing a revision or node from policy retains its disks and marks its entry
`Retained`. Garbage collection and automatic default image promotion are not
implemented. Do not delete a seed until dependent clone/snapshot behavior is
verified. Existing instance data must never be repaired by replacing its root
with a seed.

## Validation

Unit tests use a fake Kubernetes client to check immutable image policy,
architecture filtering, import concurrency across restart, node-specific binding,
PV ownership/locality checks, cache-loss repair, bounded retries, revision
retention, and refusal to adopt unrelated volumes. They do not validate CDI,
TopoLVM or recert against the real cluster. The Tekton pipeline has been
admitted and run successfully on the supplied host. The runtime Deployment is
healthy under its service account with `images: []`; no cache imports or guest
VMs are enabled. Provisioning unit tests additionally check durable unique
identities, retry/exhaustion behavior, local source placement, minimum root
capacity, TLS passthrough, independent SSH host/client keys, password hashes,
and separation of management private keys from guest configuration media.
See [build provenance](docs/build-provenance.md).
