# Operator build provenance

Verified on 2026-10-28 using the supplied host kubeconfig. All builds use Tekton
in `sno-vm-build`; the operator image repository is public.

| PipelineRun | Source commit | Published digest |
| --- | --- | --- |
| `sno-vm-operator-build-9s9jw` | `07bbae402f2303041fefcc732be86820ba5d02f6` | `sha256:07057c84bacc20f23d17988548a0aa9a9fc4ecbd6b32728b2f296df35177108a` |
| `sno-vm-operator-build-jrbj5` | `f82d5e75fd8c9c60fe1fb04274fbe7ebadb27cf4` | `sha256:e14816ba6984908bdbfb7aeeb62a6385e98ff15933c008abec5c899310e7c36c` |
| `sno-vm-operator-build-20d708f` | `20d708f17889d62a5201ca51dd0b2d258ada0026` | `sha256:4986805b5050c10a43c6b97a16a0deb058392f43b4a03706874fafa9b6f12665` |

## Current deployed build

The latest run completed both verify and publish tasks successfully. Its tag is
`quay.io/klape/sno-vm-operator:git-20d708f17889`; anonymous
`skopeo inspect --no-creds` resolved the recorded digest. The runtime Deployment
`sno-vm-system/sno-vm-operator` was updated to that digest and reports one updated,
available replica. The manifest pins the same digest.

The verify task fetched the exact source commit, ran `go mod verify`,
`go test -race ./...`, `go vet ./...`, and built a static Linux amd64 manager.
The publish task built and pushed the OCI image using the authorized Quay
credentials, mounted only in that task. The build used worker LVM storage on
`a100-01`.

The current image includes the cache controller and tested provisioning
components: the experimental API, durable allocations, resource builders, and
independent bootstrap credential generation. The complete instance reconciler
is not wired into the manager; `instances.enabled: true` is rejected explicitly.
The runtime still has `images: []`. No seed imports or ordinary SNO instances
have been enabled by this deployment.

The disposable reference guest has separately passed API, node, operator,
internal application route, and cached-engine checks. That does not qualify
recert cloning, per-node cache usability, or startup performance. Those require
the two-clone proof and complete provider integration.

Later source-only administrative proof tools are not included in this recorded
runtime image. New runtime code requires another successful Tekton build and a
new pinned deployment digest.
