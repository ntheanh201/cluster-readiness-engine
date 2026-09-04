# ADR-076: Air-Gapped Catalog Distribution Model (Design Note)

> **Status:** Proposed
> **Issue:** #243

## Context

A significant share of GPU clusters run disconnected from the internet by policy. For those clusters NVCRE today has half an answer:

- **The controller install is air-gappable.** The Helm chart and the controller image live on GHCR and are mirrorable, and `nvcrectl setup images` (this issue) emits the complete image manifest — controller, Kubeflow Trainer stack, every catalog workload image, the two OCI charts, and the Dockerfile build bases — so an operator can mirror everything in one step and point `setup init` at the mirror.
- **The catalog is not.** The certification catalog (the test definitions and their thresholds) is compiled into the `nvcrectl` binary and the controller via `//go:embed all:entries` ([`pkg/catalog/loader.go`](../../pkg/catalog/loader.go)). The only way to change it is to build a new binary. An air-gapped operator can install NVCRE but has no supported way to bring site-specific catalog entries in, adjust thresholds to their hardware, or receive catalog updates — without, at best, rebuilding the CLI behind the firewall.

Closing that gap is an architecture decision about a distribution model, not a bug fix. The options below differ materially in trust model, offline-update ergonomics, and how much of the catalog pipeline gets rebuilt; the maintainers should choose deliberately. This note documents the options and their tradeoffs. **It does not select one and this repository does not implement any of them yet.**

### What the catalog is today

- Entries are YAML templates under `pkg/catalog/entries/{domain}/{variant}.yaml`, embedded at compile time and registered by `init()` ([ADR-010](010-certification-catalog.md)). Shared `_lib/` fragments (platform env, runtime patches, dependencies) are pulled in through template helpers.
- Thresholds are CEL expressions carried in `spec.validation.performance.thresholds`, templated from the same entries and evaluated by the Job controller ([ADR-021](021-performance-threshold-enforcement.md)).
- `nvcrectl certification render` renders the embedded catalog offline; the Certification controller renders the same entries server-side.
- Overrides (platform × GPU architecture) are already data, resolved at render time — a distributed catalog would carry the same shape.

## Options Considered

### Option A — Filesystem catalog path (deterministic load order, signature-verified directory)

Ship the catalog as files on disk instead of (or in addition to) the embedded tree, loaded through a documented overlay order (site entries over released entries). Files are distributed however the site already distributes files: a signed tarball, a git mirror, an RPM/deb package, or a mounted ConfigMap.

- **Trust model:** signature or checksum verification of the catalog bundle before load; the site owns its key.
- **Offline updates:** replace files + restart the controller; no registry infrastructure needed.
- **Costs:** two catalog sources (embedded + filesystem) need a deterministic merge story, including for the shared `_lib/` fragments; the controller Deployment must mount the catalog somewhere; drift between the compiled-in and on-disk copies becomes a support surface. Rendering must become identical in both the CLI and the controller, which today share the embedded tree.

### Option B — Catalog as OCI artifacts in a mirrored registry

Define a catalog bundle as an OCI artifact (the same registry infrastructure the images already use), with a documented repository layout and version scheme. An air-gapped site mirrors one more artifact type; `nvcrectl` and the controller gain a documented override (flag/env/CR field) naming the mirror and version.

- **Trust model:** OCI registry auth + digest pinning; aligns with how every other NVCRE artifact is already mirrored.
- **Offline updates:** re-mirror a tagged artifact; version pinning makes rollbacks trivial.
- **Costs:** requires the controller to pull OCI at startup (or an agent to), which adds a runtime dependency and failure mode to every reconcile; needs a defined artifact schema (entries + `_lib` + thresholds + metadata) and tooling to build/verify it; cold-start (before the mirror is populated) needs a fallback story.

### Option C — Kubernetes-native catalog CRDs

Introduce catalog CRs (e.g. a namespaced or cluster-scoped `CatalogEntry` type) that the controller merges with the embedded catalog. Distribution becomes "apply YAML", which every Kubernetes toolchain already does; updates are ordinary `kubectl apply`/GitOps.

- **Trust model:** RBAC on the CRD — whoever can write catalog CRs controls what runs on the GPUs. That is a deliberate elevation of an existing privilege and needs its own review.
- **Offline updates:** plain YAML; works with any GitOps flow already inside the air gap.
- **Costs:** a new API surface to version, validate, and document; CEL/template rendering of CR-sourced entries must be hardened (templates are currently repo-trusted); precedence between CRs and embedded entries must be specified; the CRD must carry everything `_lib` fragments express today (dependencies, runtime patches, thresholds).

### Option D — Ship site-specific binaries

Formalize what exists de facto: an documented build profile (`-ldflags` or a build tag) that compiles a site's catalog overlay into the binary; the site builds and mirrors its own `nvcrectl`/manager images.

- **Trust model:** unchanged — the binary is the trust root.
- **Offline updates:** rebuild + re-mirror images; slow but simple.
- **Costs:** every catalog change is an image release; the site must run a Go toolchain and track upstream; no path to upstream catalog updates without rebasing the site overlay. Cheapest to reach, worst long-term.

## Tradeoff Summary

| | A: Filesystem | B: OCI artifact | C: Catalog CRDs | D: Site builds |
|---|---|---|---|---|
| New infrastructure | none | reuses image registry | CRD + RBAC review | Go toolchain at site |
| Update latency | file replace + restart | re-mirror artifact | `kubectl apply` | image release |
| Trust enforcement | bundle signature | registry + digests | RBAC | binary itself |
| Controller coupling | mount + load order | runtime pull or agent | watch + merge | none |
| Upstream update path | good | good | good | rebase |
| Works for CLI **and** controller | yes (same files) | yes | controller-native; CLI reads CRs or files | yes |
| Schema/UX design needed | overlay order | artifact schema | CRD schema + precedence | build profile |

## Decision

**None yet — deliberately.** This note exists so the maintainers can weigh the options against the trust model they want for "who decides what burns the GPUs". The image-manifest work from this issue (the `setup images` command, the completeness test, and the air-gap install procedure) is intentionally independent of the choice: every option still needs the same mirror manifest, and none of them change what `nvcrectl setup init` installs today.

Whichever option is chosen, the ADR that lands it should specify at minimum:

1. The exact catalog payload format (entries, `_lib` fragments, thresholds, metadata) and its versioning.
2. Precedence and merge semantics against the embedded catalog, including partial overlays.
3. The trust chain: who signs/reviews catalog content and how the air-gapped side verifies it.
4. How `nvcrectl certification render` and the controller stay rendering-identical on the same catalog.

## Consequences

- Until a model is chosen, air-gapped sites can install, run the released catalog, and tear down NVCRE; they cannot change thresholds or add entries without building binaries.
- The image manifest must be regenerated (and will be caught by its completeness test) if any option later adds new images (e.g. a catalog-pull sidecar for Option B/C).

## Alternatives Considered

- Doing nothing: the released catalog is fixed but functional; sites that need changes rebuild binaries unofficially. Rejected as a long-term answer — "no supported way" is exactly the gap the issue names.
- Picking Option B now because it matches the existing registry story: rejected for this issue — the runtime-pull coupling to the controller reconcile loop deserves its own review, and the maintainer decision was explicitly out of scope.

## References

- Issue #243 — Air-gapped install (this issue; scoped to the image manifest plus this design note).
- ADR-010 — Certification catalog with `init()` registration (the embedded-catalog mechanics).
- ADR-064 / ADR-065 — Helm chart distribution and `nvcrectl` Helm install (the mirror precedent this note builds on).
- ADR-021 — Performance threshold enforcement (the thresholds any distributed catalog must carry).
- `docs/operations/air-gapped-install.md` — the proposed install/teardown procedure (documented, not yet executed against a real egress-denied cluster) and the `nvcrectl setup images` manifest.
