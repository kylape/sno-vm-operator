# Operator build provenance

Verified on 2026-11-05 using the supplied host kubeconfig. All builds use Tekton
in `sno-vm-build`; the operator image repository is public.

| PipelineRun | Source commit | Published digest |
| --- | --- | --- |
| `sno-vm-operator-build-9s9jw` | `07bbae402f2303041fefcc732be86820ba5d02f6` | `sha256:07057c84bacc20f23d17988548a0aa9a9fc4ecbd6b32728b2f296df35177108a` |
| `sno-vm-operator-build-jrbj5` | `f82d5e75fd8c9c60fe1fb04274fbe7ebadb27cf4` | `sha256:e14816ba6984908bdbfb7aeeb62a6385e98ff15933c008abec5c899310e7c36c` |
| `sno-vm-operator-build-20d708f` | `20d708f17889d62a5201ca51dd0b2d258ada0026` | `sha256:4986805b5050c10a43c6b97a16a0deb058392f43b4a03706874fafa9b6f12665` |
| `sno-vm-operator-build-mmq9h` | `9950fe27df07f1f246df0a1dad587f9317bef0b1` | `sha256:43a92d5e567bd8da14fb8180278ea0f621ab77fb5c4bec7dab9562efa4fcb6ae` |
| `sno-vm-operator-build-76bcb` | `1f3ee55c015a22daceb0d4e7a00b3151fa7d4e70` | `sha256:de69de2e498d9faa9713e966f17d0690a3ddd5bce2cbe42888bf5da2c58f7311` |
| `sno-vm-operator-build-xtgh5` | `0143eb2fa0b23fb05eef051cba8c1b7f95b8f7c3` | `sha256:fc9543451b6c0aaed210be89d214fb68f8d66e404830fe3a81c256ea7e8dab22` |
| `sno-vm-operator-build-grfwn` | `88de09483b2542ce4ac36ecd48c417af5638a114` | `sha256:dd54776c0879705b144cd5982ca2bebf28b4e698e4186fd7f4cc91ab9b99660e` |

## Current deployed build

The latest run completed both verify and publish tasks successfully. Its tag is
`quay.io/klape/sno-vm-operator:git-88de094`; the runtime Deployment
`sno-vm-system/sno-vm-operator` was updated to the recorded digest and reports one
available replica. The manifest pins the same digest.

The verify task fetched the exact source commit, ran `go mod verify`,
`go test -race ./...`, `go vet ./...`, and built a static Linux amd64 manager.
The publish task built and pushed the OCI image using the authorized Quay
credentials, mounted only in that task. The build used worker LVM storage on
`a100-01`.

The current image includes the SNO instance reconciler, Docker config Secret
conversion for CDI registry imports, source validation that resolves the same
converted CDI Secret reference used by the cache controller, validation of the
separate guest SSH client and host key pairs, a short-lived binder Pod for
WaitForFirstConsumer root PVCs, and the OpenShift ClusterVersion health check
using its `Failing` condition. The Tekton verification task passed
`go test -race ./...`, `go vet ./...`, and the static Linux amd64 build.

The direct smoke-test CR `sno-vm-system/sno-direct-smoke-20261104` is Ready.
Its seed cache was Ready on `a100-01`; the root binder selected that worker,
CDI completed the 50 GiB seed clone into a 500 GiB root, and KubeVirt reports
the VM Ready. OpenShift admitted the passthrough API Route. The operator
verified the guest cluster ID, API readiness, the single Ready node, and all 34
ClusterOperators, then published the guest kubeconfig in the request's
`-credentials` Secret.

The disposable reference guest has separately passed API, node, operator,
internal application route, and cached-engine checks. That does not qualify
recert cloning, per-node cache usability, or startup performance. Those require
the two-clone proof and complete provider integration.

Later source-only administrative proof tools are not included in this recorded
runtime image. New runtime code requires another successful Tekton build and a
new pinned deployment digest.
