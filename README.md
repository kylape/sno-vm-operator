# SNO VM operator

Provision preinstalled Single Node OpenShift VMs on OpenShift Virtualization.
Users will request SNO environments through the LLM Workload Environment
operator. This repository owns the SNO runtime controller and its Tekton build;
[image preparation and the approved design](https://gitlab.cee.redhat.com/klape/inference-engineering-scratchpad/-/tree/main/sno-virt-image)
are maintained separately.

## Implementation status

The operator now includes the `sno.llm-d.io/v1alpha1` `SNOVirtualMachine`
reconciliation path: durable allocations, local disk cloning, KubeVirt VM and
API Route creation, guest identity/readiness checks, kubeconfig publication,
and deletion cleanup. That implementation is built and deployed on the
development cluster. A cluster administrator must still install the API and
runtime RBAC before requests can create VMs; no guest has been launched by this
operator yet.

The disposable reference SNO is installed and healthy, with verified external
API TLS/readiness and an internally reachable application route. The LWE SNO
adapter is on the `feature/sno-mode-integration` branch and has been built and
deployed. End-to-end VM launch and readiness through an LWE request remain to
be validated. No five-minute startup result is claimed.

The host currently uses TopoLVM without snapshot classes; CDI selects `copy`.
A cached seed avoids registry downloads but still requires a measured clone
benchmark. Do not alter existing LVM device classes to enable thin provisioning
without a separate storage decision.

## Develop

Go 1.26 is required. Run `make test` and `make build`. The controller starts with
`bin/manager --config=config/cache.example.yaml` and an ordinary kubeconfig or
in-cluster service account. Running the controller creates Kubernetes resources;
use an approved target cluster. The image is Linux amd64 initially.

`kubectl kustomize config` renders the CRD, runtime RBAC, Deployment, and cache
policy together. A cluster administrator must install the CRD initially. The
development identity can update the named CRD after creation but cannot create
CRDs. To install only the API and VM lifecycle permissions on an existing
operator deployment, a cluster administrator applies:

```sh
kubectl apply -f config/crd.yaml
kubectl apply -f config/rbac/operator-runtime.yaml
```

The runtime role lets the operator manage its `SNOVirtualMachine` requests and
the VM, CDI, Route, Service, PVC, and Secret resources needed for guest
provisioning. It is cluster-bound so LWE requests can run in their backing
namespaces.

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
Quay digest. Copy the policy and supply the digest, `50Gi` seed disk size, and the
read-only registry Secret name. Use one placement per worker storage pool.
The cache must retain the compact source capacity; importing it into a 500 GiB
PVC would restore the full-device copy cost. Provisioned guests can still use
500 GiB roots, with partition and filesystem growth before specialization.
Nodes matching multiple placements are rejected. Only amd64 nodes matching the
configured worker selectors participate. Cordoned, unready, or untolerated nodes
remain pending in coverage.

For a single administrator-run development test, `instances.preloadedImages`
can reference an already validated candidate PVC by namespace, name, UID, node,
and storage class. The operator checks the CDI/PVC/PV ownership, block mode,
capacity, and node locality before cloning it. This path bypasses registry
import and is intended for controlled candidate testing; production policy
should use digest-pinned `images` and the per-node cache. Guest bootstrap also
requires a separate guest-only pull Secret in the configured namespace.

For CDI `pullMethod: pod`, `registrySecret` names a Secret in `sno-vm-system`.
It can use CDI's Opaque `accessKeyId`/`secretKey` username/password fields or a
standard Docker config JSON (`.dockerconfigjson` or `config.json`). For Docker
config JSON, the operator selects the credentials matching the image registry
and creates an owned CDI-format Secret for the import. The source Secret remains
unchanged. The same Docker config Secret can also be referenced by
`instances.guestPullSecret` for guest bootstrap. Credentials must include pull
access to the Quay seed repository; build push credentials serve a separate
purpose.

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
admitted and run successfully on the supplied host. Provisioning unit tests
check durable unique identities, retry behavior, local source placement,
minimum root capacity, TLS passthrough, independent SSH host/client keys,
password hashes, and separation of management private keys from guest
configuration media. The compact seed remains unqualified for production use;
the preloaded profile is an explicit development-only opt-in.
See [build provenance](docs/build-provenance.md).
