---
title: nvcrectl setup
description: Install and uninstall the NVIDIA Cluster Readiness Engine controller and its dependencies.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## nvcrectl setup init

Installs NVCRE via Helm and its dependencies on the target cluster.

```bash
nvcrectl setup init [flags]
```

### What it installs

Runs two phases in order:

| Phase | What |
|-------|------|
| `deps` | Kubeflow Trainer v2.2.1 |
| `helm` | NVCRE Helm chart (CRDs, controller, built-in LogProfiles) pulled from GHCR |

Use `--skip-phases=deps` to skip Kubeflow Trainer if it is already installed.

The `helm` phase reconciles the NVCRE CRDs on every run: it extracts the CRD manifests from the same chart version it is about to install (`helm show crds`) and server-side-applies them (field manager `nvcrectl-setup`, force ownership) before running `helm upgrade --install`. Helm itself applies a chart's `crds/` directory only on the *first* install, so without this step an upgrade would leave the installed CRDs at the old schema. Server-side apply is idempotent, so a fresh install and an unchanged re-run behave exactly as before.

### Retry behavior and automatic recovery

`setup init` converges from any partial state, so re-running it after a failure is always safe to try:

- **Already deployed**: when the `kubeflow-trainer` release is `deployed` at the pinned chart version, the `deps` phase prints "already deployed" and skips the upgrade entirely. Not re-rendering the chart means not re-rolling its webhook certificates.
- **Failed or pending release**: the install is attempted once. If it fails with the webhook Secret field-ownership conflict signature (`Apply failed with ... conflicts` on `.data` fields of Secrets in `kubeflow-system`) *and* the release state agrees (`failed` or `pending-*`), `setup init` performs an automatic recovery: uninstall the release, delete its four CRDs (`trainjobs`, `trainingruntimes`, `clustertrainingruntimes` in `trainer.kubeflow.org`; `jobsets` in `jobset.x-k8s.io`), delete the `kubeflow-system` namespace, and reinstall the pinned chart. Exactly one recovery attempt is made per run.
- **Safety gate**: automatic recovery is refused when any `TrainJob` or `JobSet` instance exists, or when a `TrainingRuntime`/`ClusterTrainingRuntime` exists that is not Helm-owned (missing the `app.kubernetes.io/managed-by: Helm` label) — deleting the CRDs would destroy them. In that case, and for any ambiguous failure, `setup init` fails fast and prints the manual procedure:

  ```bash
  helm uninstall kubeflow-trainer --namespace kubeflow-system
  kubectl delete crd trainjobs.trainer.kubeflow.org trainingruntimes.trainer.kubeflow.org \
    clustertrainingruntimes.trainer.kubeflow.org jobsets.jobset.x-k8s.io
  kubectl delete namespace kubeflow-system
  nvcrectl setup init   # reinstalls the pinned Kubeflow Trainer
  ```

- **Confirmation**: in interactive mode the recovery plan is printed and re-confirmed before anything is deleted; `--auto-approve` covers CI.

<Warning>
Automatic recovery deletes the `kubeflow-system` namespace, including anything you placed there manually. The namespace is created and managed by `setup init`.
</Warning>

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--image-pull-secret` | — | GitHub token for clusters that pull from a private GHCR mirror or fork: the CLI creates a `ghcr.io` pull secret and uses it to authenticate the Helm chart pull. The public image and chart need no token. |
| `--image` | — | Override the controller image (default: `ghcr.io/nvidia/cluster-readiness-engine/manager:<version>`) |
| `--skip-phases` | — | Comma-separated phases to skip (e.g., `deps`) |
| `--version` | — | Helm chart version to install (required for dev builds) |
| `--auto-approve` | `false` | Skip the interactive confirmation prompt (for CI/automation) |

### Example

```bash
# Standard install
nvcrectl setup init

# For a private GHCR mirror or fork (the public image and chart need no token)
nvcrectl setup init --image-pull-secret $GITHUB_TOKEN

# Skip Kubeflow Trainer (already installed)
nvcrectl setup init --skip-phases=deps
```

## nvcrectl setup status

Reports the installation status of NVCRE and its dependencies by querying the cluster.

```bash
nvcrectl setup status [flags]
```

Components checked: `nvcreCRDs`, `nvcreController`, `kubeflowTrainer`, `logProfiles`, `gpuOperator`, `dcgm` (optional).

The `kubeflowTrainer` check verifies the installed Kubeflow Trainer version, not just that the TrainJob CRD exists. The version is detected from the managed Helm release chart version, the Trainer controller Deployment image tag, or the `app.kubernetes.io/version` label on the TrainJob CRD — whichever answers first — and reported under `kubeflowTrainerVersion` with its detection source. A version other than the one this NVCRE build supports fails the check with a message naming the detected and supported versions; a Trainer install whose version cannot be determined passes with a warning.

The Helm releases managed by `setup init` (`nvcre` and `kubeflow-trainer`) are also checked via the helm CLI and reported under `helmReleases`. A release in a failed or pending state (e.g. `failed`, `pending-upgrade`) makes the status not ready. A release helm has no record of, or that cannot be queried (helm not in PATH), is reported but does not affect readiness.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output` / `-o` | `table` | Output format: `table`, `json` |

### Example

```bash
nvcrectl setup status
nvcrectl setup status -o json
```

## nvcrectl setup reset

Removes NVCRE and its dependencies from the target cluster. Kubeflow Trainer is removed by default.

```bash
nvcrectl setup reset [flags]
```

### What it removes

Runs three phases in order:

| Phase | What |
|-------|------|
| `cr` | All NVCRE custom resource instances (Certifications, Workflows, Jobs) |
| `helm` | NVCRE Helm release (CRDs, controller, LogProfiles) |
| `deps` | Kubeflow Trainer |

Use `--skip-phases=deps` to keep Kubeflow Trainer.

After all phases complete, `setup reset` prints a **Retained resources** block listing any namespaces or secrets that were not deleted (Helm never removes the release namespace, and the `nvcrectl-pull-secret` created by `setup init` is outside the Helm release). Each entry includes the `kubectl delete` command to remove it manually if you want a pristine cluster.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--skip-phases` | — | Comma-separated phases to skip (e.g., `deps`) |
| `--auto-approve` | `false` | Skip the interactive confirmation prompt |

### Example

```bash
# Full uninstall (including Kubeflow Trainer)
nvcrectl setup reset

# Keep Kubeflow Trainer
nvcrectl setup reset --skip-phases=deps
```

<Warning>
`reset` deletes all Certification, Workflow, and Job resources. This is irreversible.
</Warning>

## `nvcrectl setup images`

Prints every container image and Helm chart an air-gapped install must mirror. Offline: it reads the embedded catalog, the chart values, and the controller Dockerfile, and contacts nothing.

The list is derived, not hand-maintained — catalog entries are built across every platform and GPU-architecture combination and their overrides resolved the way `certification render` resolves them, so an image used only by one platform's override block is still found.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output` | `table` | Output format: `table`, `yaml`, or `json` |
| `--image-version` | CLI version | NVCRE version for the controller image tag and chart version (`<version>` placeholder on dev builds) |
| `--trainer-version` | the pinned version | Kubeflow Trainer chart version. Fails if that version's rendered sub-chart images are not recorded in the repo — see the note below |

### Example

```bash
nvcrectl setup images --output yaml > nvcre-images.yaml
```

<Note>
Only the Trainer manager's tag follows the chart version; sub-charts such as JobSet pin their own tags and move independently (JobSet went `v0.11.0` to `v0.12.0` between Trainer 2.2.1 and 2.3.0). Passing a `--trainer-version` whose render has not been recorded in the repo is an error naming the `helm template` command that records it, rather than a manifest built from the previous version's tags — mirroring those would fail only after egress was cut.
</Note>

See [Air-Gapped Install](../operations/air-gapped-install.md) for the full mirror-and-install path, including a step that does not work yet.
