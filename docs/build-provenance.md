# Initial cache controller build

Verified on 2026-10-28 using the supplied host kubeconfig.

| Property | Value |
| --- | --- |
| Source | `07bbae402f2303041fefcc732be86820ba5d02f6` |
| Tekton namespace | `sno-vm-build` |
| PipelineRun | `sno-vm-operator-build-9s9jw` |
| Pipeline result | Succeeded; both verify and publish tasks succeeded |
| Image tag | `quay.io/klape/sno-vm-operator:git-07bbae402f23` |
| Image digest | `sha256:07057c84bacc20f23d17988548a0aa9a9fc4ecbd6b32728b2f296df35177108a` |
| Public pull | Anonymous `skopeo inspect --no-creds` resolved the same digest |
| Runtime namespace | `sno-vm-system` |
| Runtime Deployment | `sno-vm-operator`, 1/1 available |
| Enabled seed images | None (`images: []`) |

The verify task fetched the exact source commit, ran `go mod verify`,
`go test -race ./...`, `go vet ./...`, and built a static Linux amd64 manager.
The publish task built the OCI image and pushed it using the authorized Quay
credentials. Push credentials were mounted only in the publish task.

The deployed digest acquired its leader-election lease, passed its HTTP health
probes, and wrote the empty cache status ConfigMap using its runtime service
account. There were no reconciliation errors in the observed startup logs.
This validates runtime startup and the empty policy only. It does not validate
imports, local volume binding, clone speed, recert, or SNO provisioning.

Subsequent documentation and deployment-manifest commits can reference this
image without rebuilding its unchanged manager binary. New controller code
requires a new successful Tekton build and a new pinned deployment digest.

## Current deployed cache build

The subsequent successful `sno-vm-operator-build-jrbj5` PipelineRun built source
`f82d5e75fd8c9c60fe1fb04274fbe7ebadb27cf4` and published
`quay.io/klape/sno-vm-operator:git-f82d5e75fd8c` with digest
`sha256:e14816ba6984908bdbfb7aeeb62a6385e98ff15933c008abec5c899310e7c36c`.
The deployment manifest pins that build. It includes the corrected nonroot pause
binder and has run with an empty image policy.

Later source adds the experimental instance API, policy, allocations, resource
builders, credential generation, and unit tests. Those changes are not part of
the deployed digest. The manager explicitly rejects instance enablement until
the complete reconciliation/readiness/deletion flow is available.

Host API timeouts recurred during reference work on 2026-10-28. A new Tekton run
and runtime rollout are deferred until host stability permits reliable build
execution. Passing local tests does not constitute a new published image.
