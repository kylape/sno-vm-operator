# Administrator tracer bullet

The initial 500 GiB reference established the first working clone. A corrected
50 GiB seed now passes two independent first boots and an identity-preserving
reboot, using 500 GiB destination disks on worker LVM storage. See the
[measured verification results](https://gitlab.cee.redhat.com/klape/inference-engineering-scratchpad/-/blob/main/sno-virt-image/verification.md).
Clone/scheduling takes about four minutes with current CDI defaults; the
five-minute API/application target remains unfinished.
This administrative procedure qualifies the image; ordinary users should use
the LWE/provider API once its complete reconciler is enabled.

## Prepare and capture the reference

The [image preparation code](https://gitlab.cee.redhat.com/klape/inference-engineering-scratchpad/-/tree/main/sno-virt-image)
installs the reference, renders the health application, checks API/operator/node
and application readiness, caches immutable etcd/recert images, and records
metadata with `cloneReady: false`.

After qualification, run `guest/prepare_cert_links.py` on the disposable guest.
It refuses differing live/revision etcd certificates. Set the VM to Manual,
install the startup gate with `guest/install_gate.py`, verify the systemd unit,
and power off cleanly. Wait until the VMI is absent before exporting its root.
Do not export the installer ISO.

The tested capture uses the pinned virtctl `vmexport download` command with
`--pvc=<root> --format=gzip --port-forward --keep-vme`. Use a protected output
path and retain the original powered-off source PVC. After download completes,
delete the temporary VirtualMachineExport and wait for its exporter Pod to exit.

Run `scripts/sparse_inflate.py` with the exact expected virtual capacity, then
convert the validated raw disk to a standalone compressed qcow2 with `qemu-img`.
Run both `qemu-img check` and `qemu-img compare` against the exported raw disk.
`scripts/build_disk_oci.py` packages the checked qcow2 as an OCI container disk
owned by 107:107 with mode 0440. The candidate remains unqualified and must be
pushed only to the private `quay.io/klape/sno-vm-image` repository.

For compact seeds, use `scripts/compact_seed.py` to repack the captured contents
into a 50 GiB filesystem. Native builders can recover the XFS journal first on
an exclusive disposable copy with `scripts/recover_seed_copy.py`. Validate the
repacked filesystem, convert it to qcow2, and compare that qcow2 with the repacked
raw disk. Its new metadata records `diskSize: 50Gi` and a fresh seed ID. The
source cache PVC must be 50 GiB; a 500 GiB cache would retain the larger copy cost.
The guest gate expands the copied root partition and XFS before specialization.

## Render one disposable clone

The offline renderer does not connect to Kubernetes or promote an image:

```bash
go run ./cmd/render-seed-proof \
  --name sno-proof-one --namespace sno-seed-build \
  --node a100-01 --storage-class lvms-a100-tier1-storage \
  --source-pvc sno-seed-compact50 --source-namespace sno-seed-build \
  --metadata /protected/compact-metadata.json \
  --pull-secret /protected/pull-secret.json \
  --ssh-public-key /protected/administrator.pub \
  --dns 172.30.32.118 --machine-cidr 10.252.1.0/24 \
  --output /protected/proof-one
```

It creates a new directory (0700) and files (0600), refuses overwrites, and
records a fresh identity in `allocation.json`. The management Secret contains
canonical bootstrap material for recovery, but the guest receives only its
configuration Secret. Operator SSH client private keys are not mounted in the
VM. Candidate manifests have no production SNO owner reference or image policy
promotion and initially keep the VM halted.

Create `bootstrap-secrets.yaml` and `clone-resources.yaml`. Use the resulting
access Service IP with `scripts/render_dns_zone.py` to add only this guest's
zone to the existing DNS ConfigMap. Start the VM and observe CDI until its disk
clone succeeds; do not replace or restart an established specialization attempt
with different configuration.

## Verify the tracer bullet

Once the VMI boots, first confirm the root filesystem expanded to the requested
capacity. Then verify:

1. Pin the generated SSH host public key from the management Secret and connect
   to guest port 22 through its internal access Service. Do not accept an
   arbitrary host key or reuse the reference's host key.
2. Read `/var/lib/sno-init/complete.json`. Its instance UID, requested cluster
   ID, and SHA-256 of `config.json` must match the protected allocation and
   bootstrap material.
3. Retrieve only the current administrator kubeconfig from the guest's
   `lb-ext.kubeconfig` using `oc config view --raw --flatten`. Keep its contents
   outside Git and logs. Use the external HTTPS Route URL on port 443 and its
   regenerated embedded guest CA. The CA bundle must differ from the source.
4. Verify the externally accessible API and the single node's allocated
   hostname. Reconcile `ClusterVersion.spec.clusterID` to the allocated UUID
   before reporting readiness: recert does not set this field itself.
5. Reconcile the health Route hostname and, if needed, roll its deployment to
   replace source-bound Pod state. A host-cluster request through the access
   Service port 80 with the guest route's Host header must return `sno-ready`.
6. Using the new guest CA and endpoint but the original source administrator
   credentials, request a protected API such as `/api/v1/namespaces`. Require
   authentication rejection. Public health endpoints are insufficient for
   this test; network failures do not establish credential isolation.

One working clone completes this tracer bullet. A second independent clone,
cross-cluster credential rejection, restart behavior, complete provider/LWE
orchestration, all-eligible-node cache coverage, and startup measurements remain
required before production image enablement. No five-minute result is claimed
by this administrative proof.
