---
title: Air-Gapped Install
description: Mirror every image and chart NVCRE needs into a private registry, and what does and does not yet work when installing from it with egress denied.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---

NVCRE runs in clusters that are disconnected from the internet by policy. The
install is Helm-based, so the images are mirrorable; this page walks through
the path: mirror, install, certify, tear down. Read the Install section first —
one step of it does not work yet.

## Generate the mirror manifest

One command prints every artifact an air-gapped install needs:

```bash
nvcrectl setup images --output yaml > nvcre-images.yaml
```

or, from a repo checkout:

```bash
make airgap-images
```

The manifest is derived from the same sources the running system uses:

| Kind | What | Where it comes from |
|------|------|---------------------|
| `controller` | `ghcr.io/nvidia/cluster-readiness-engine/manager:<version>` | the Helm chart's `manager.image` and the `setup init --image` default |
| `trainer` | Kubeflow Trainer controller and JobSet controller | the chart pinned by `setup init`'s `[deps]` phase |
| `workload` | NGC PyTorch, DCGM, nccl-tests, GCP TCPXO daemon | every catalog entry, resolved across all platform and GPU-architecture overrides |
| `build` | Go builder and distroless base | the controller Dockerfile |

plus the two OCI Helm charts (`oci://ghcr.io/nvidia/cluster-readiness-engine`
and `oci://ghcr.io/kubeflow/charts/kubeflow-trainer`) with the versions the
install pulls.

On a release build the controller image tag and the NVCRE chart version equal
the CLI version. On a dev build pass them explicitly:

```bash
nvcrectl setup images --image-version v1.2.3 --output yaml
```

## Mirror to a private registry

Copy every image and chart in the manifest into a registry reachable from the
cluster. For each container image:

```bash
skopeo copy --all docker://<image> docker://<mirror>/<path-after-registry>
```

and for the Helm charts:

```bash
helm pull oci://ghcr.io/nvidia/cluster-readiness-engine --version <version>
helm push cluster-readiness-engine-<version>.tgz oci://<mirror>/<namespace>
helm pull oci://ghcr.io/kubeflow/charts/kubeflow-trainer --version <version>
helm push kubeflow-trainer-<version>.tgz oci://<mirror>/<namespace>
```

Preserve image tags when mirroring — the rendered workload specs reference
images by name and tag.

## Install

<Warning>
`nvcrectl setup init` cannot yet install from a mirrored chart. It overrides
the controller **image** with `--image`, but both Helm chart locations are
compile-time constants pointing at `ghcr.io`
([`helmChartOCI` and `trainerHelmChartOCI`](../../pkg/setup/helm.go)), and
`--image-pull-secret` authenticates against `ghcr.io` specifically. With egress
denied, `setup init` therefore fails when it pulls its own chart. Installing
NVCRE itself from a mirror needs chart-location overrides that do not exist
today; until they land, only the Kubeflow Trainer half of the install can be
served from a mirror, using the `--skip-phases=deps` route below. Mirroring the
NVCRE chart is still worth doing — the manifest lists it — so the mirror is
complete when those overrides arrive.
</Warning>

Install the pinned Trainer chart from the mirror first, then run `setup init`
with the `[deps]` phase skipped and the controller image pointed at the mirror:

```bash
helm upgrade --install kubeflow-trainer \
  oci://<mirror>/<namespace>/kubeflow-trainer \
  --namespace kubeflow-system --create-namespace \
  --version 2.2.1 \
  --set manager.tolerations[0].operator=Exists \
  --set jobset.controller.tolerations[0].operator=Exists
nvcrectl setup init --skip-phases=deps \
  --image <mirror>/cluster-readiness-engine/manager:<version>
```

Workload images (the `workload` kind in the manifest) are pulled by the
certification pods themselves. Mirror them to the same registry, or reference
mirrored equivalents from the Certification spec.

## Verify

`nvcrectl setup status` reports component readiness without network access
beyond the cluster itself. The components that depend on mirrored images are
`nvcreController` and `kubeflowTrainer`; a pod stuck in `ImagePullBackOff`
means an image was missed by the mirror.

## Update

To update NVCRE in the air-gapped environment: re-run `nvcrectl setup images`
with the new version, mirror the deltas (image tags change; the chart version
changes), and re-run the install with the new `--image`. The catalog workload
images change only when the catalog entries change — diff the manifests to see
exactly what moved.

## Validate the whole path with egress denied

Before trusting the mirror, validate install → certify → tear down in an
environment where egress is provably denied:

1. **Deny egress.** Run the validation inside a network namespace or VLAN with
   no route to the internet. To prove denial rather than assume it, resolve
   the public registries up front (`getent hosts ghcr.io nvcr.io
   registry.k8s.io public.ecr.aws us-docker.pkg.dev`) and confirm every
   connection from a cluster node times out afterwards.
2. **Preload the mirror** with everything the manifest lists — not a subset.
   The manifest is the source of truth; "it worked last time" hides misses.
3. **Install** by the `--skip-phases=deps` route in the previous section. Note
   that this step is where the missing chart-location override bites: confirm
   what actually happens rather than assuming it succeeds. Run
   `nvcrectl setup status` and confirm every component is ready.
4. **Certify** a small target (a `Certification` with a single
   `communication/nccl-loopback` category on one GPU node is enough; which
   image it pulls depends on the detected platform, so check the manifest's
   `source:` entries for that category rather than assuming a single image). Confirm the workload
   pods reach `Running` and the Certification reaches a terminal condition.
5. **Tear down** with `nvcrectl setup reset` and confirm the release, the
   CRDs, and the Trainer install are removed.
6. **Repeat 3–5 once more** to prove the mirror also covers the update path
   (re-running install against existing state).

A failure at step 4 with an `ImagePullBackOff` pod names the missed image
directly: add it to the mirror and re-run.

## Remove

```bash
nvcrectl setup reset
```

This removes the NVCRE custom resources, the controller, the CRDs, and
Kubeflow Trainer. Pass `--skip-phases=deps` to keep Kubeflow Trainer.
