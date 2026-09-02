// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Air-gap image manifest (issue #243). Derives every container image an
// air-gapped NVCRE install must mirror, from the same sources the running
// system uses:
//
//   - the controller image, from this package's default registry and
//     repository (what `nvcrectl setup init` installs),
//   - the Kubeflow Trainer stack, from the pinned trainer chart version and
//     the images the chart renders with `setup init`'s exact values,
//   - every catalog workload image, by building each registered catalog entry
//     across a platform × GPU-architecture matrix and resolving platform and
//     architecture overrides exactly like `nvcrectl certification render`,
//   - the Helm chart OCI references (install-time, not container images, but
//     equally mirror-required), and
//   - the NVCRE controller build base images from the repo Dockerfile
//     (embedded at compile time).
//
// The single entry point is BuildManifest, so `nvcrectl setup images` and the
// repo's completeness test cannot diverge.
package setup

import (
	"bufio"
	"embed"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/render"
)

//go:embed testdata/dockerfile
var dockerfileFS embed.FS

// ImageRef is one entry of the mirror manifest.
type ImageRef struct {
	// Image is the full image reference (registry/repository:tag, optionally
	// with @digest).
	Image string `json:"image"`
	// Source names where the reference comes from.
	Source string `json:"source"`
	// Kind classifies the reference: "controller", "trainer", "workload",
	// or "build".
	Kind string `json:"kind"`
}

// ChartRef is one Helm chart OCI reference an operator must mirror.
type ChartRef struct {
	// Chart is the OCI reference, e.g. oci://ghcr.io/nvidia/cluster-readiness-engine.
	Chart string `json:"chart"`
	// Version is the version tag to mirror.
	Version string `json:"version"`
	// Source names where the reference comes from.
	Source string `json:"source"`
}

// ImageManifest is the complete set of artifacts an air-gapped environment
// needs.
type ImageManifest struct {
	Images []ImageRef `json:"images"`
	Charts []ChartRef `json:"charts"`
}

// ImageManifestOptions tunes manifest generation.
type ImageManifestOptions struct {
	// Version is the NVCRE version. The controller image tag and the NVCRE
	// chart version both default to it, matching `nvcrectl setup init`.
	// Empty means a "<version>" placeholder.
	Version string
	// TrainerVersion overrides the pinned Kubeflow Trainer chart version.
	// Empty means the version `setup init` pins.
	TrainerVersion string
}

// platformArch is one point of the platform × GPU-architecture sweep.
type platformArch struct {
	Platform string
	GPUArch  string
}

// imageMatrix is the platform × GPU-architecture space the catalog image
// derivation sweeps. Both axes come from real repo surfaces: platforms and
// architectures from the embedded node templates under pkg/render/nodes
// (each name is a <platform>-<arch>.yaml file). Sweeping the matrix instead
// of grepping template text resolves every platform and architecture override
// exactly the way the controller does, so images used only by, say, the AWS
// GB300 RoCE block are found without hardcoding them here.
func imageMatrix() ([]platformArch, error) {
	pairs, err := render.AvailableNodeTemplates()
	if err != nil {
		return nil, fmt.Errorf("list embedded node templates: %w", err)
	}
	out := make([]platformArch, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, platformArch{Platform: p.Platform, GPUArch: p.GPUArch})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Platform != out[j].Platform {
			return out[i].Platform < out[j].Platform
		}
		return out[i].GPUArch < out[j].GPUArch
	})
	return out, nil
}

// imageNodeCounts are the per-job node counts the sweep tries, smallest first.
// Node count does not appear in any image field, but it does gate whether an
// entry builds at all: training entries reject a job too small to hold their
// parallelism (nemotron5-56b needs 32 GPUs, so 4 nodes of 8). Trying only 1
// node made that entry build for zero combinations and contribute zero images.
var imageNodeCounts = []int32{1, 2, 4, 8, 16}

// imageBuildConfig returns the catalog BuildConfig used for the sweep. GPU
// count comes from the architecture default; checkpoint, thresholds, and
// iterations do not appear in any image field.
func imageBuildConfig(pa platformArch, nodesPerJob int32) catalog.BuildConfig {
	defaults := catalog.GPUDefaults(pa.GPUArch, pa.Platform)
	gpusPerNode := defaults.GpusPerNode
	if gpusPerNode <= 0 {
		gpusPerNode = 1
	}
	return catalog.BuildConfig{
		GPUArchitecture: pa.GPUArch,
		NodesPerJob:     nodesPerJob,
		GpusPerNode:     gpusPerNode,
		MlnxPerNode:     defaults.MlnxPerNode,
	}
}

// buildForImages builds entry at the smallest node count it accepts. The
// returned error is the last failure when no count works.
func buildForImages(entry *catalog.Entry, target nvcrev1alpha1.TargetSpec, pa platformArch) (nvcrev1alpha1.WorkflowSpec, error) {
	var lastErr error
	for _, n := range imageNodeCounts {
		spec, err := entry.Build(target, imageBuildConfig(pa, n))
		if err == nil {
			return spec, nil
		}
		lastErr = err
	}
	return nvcrev1alpha1.WorkflowSpec{}, lastErr
}

// trainerSubchartImages records the images the Kubeflow Trainer chart's
// sub-charts render, keyed by the trainer chart version. Only the trainer
// manager's own tag can be derived from the version; sub-charts pin their own
// tags in their values, and those move independently — jobset went v0.11.0 ->
// v0.12.0 between trainer 2.2.1 and 2.3.0.
//
// This table is transcribed from a real render, which is the only source of
// truth:
//
//	helm template kubeflow-trainer oci://ghcr.io/kubeflow/charts/kubeflow-trainer \
//	  --version <v> --set manager.tolerations[0].operator=Exists \
//	  --set jobset.controller.tolerations[0].operator=Exists
//
// The LWS sub-chart image renders only when data cache is enabled, which
// `setup init` does not enable, so it is deliberately absent.
//
// An unknown version is an error rather than a guess: emitting a stale
// sub-chart tag would put an operator through mirror -> cut egress ->
// ImagePullBackOff, which is exactly the failure this manifest exists to
// prevent.
var trainerSubchartImages = map[string][]string{
	"v2.2.1": {"registry.k8s.io/jobset/jobset:v0.11.0"},
}

// trainerChartImages returns the images the Kubeflow Trainer chart renders at
// chartVersion with `setup init`'s helm args. The manager tag follows the
// version; sub-chart tags come from trainerSubchartImages.
func trainerChartImages(chartVersion string) ([]string, error) {
	if chartVersion == "" {
		chartVersion = kubeflowTrainerVersion
	}
	if !strings.HasPrefix(chartVersion, "v") {
		chartVersion = "v" + chartVersion
	}
	sub, ok := trainerSubchartImages[chartVersion]
	if !ok {
		return nil, fmt.Errorf(
			"no recorded sub-chart images for Kubeflow Trainer %s: render the chart and add its "+
				"sub-chart images to trainerSubchartImages in pkg/setup/images_manifest.go "+
				"(helm template kubeflow-trainer oci://ghcr.io/kubeflow/charts/kubeflow-trainer "+
				"--version %s --set manager.tolerations[0].operator=Exists "+
				"--set jobset.controller.tolerations[0].operator=Exists | grep image:)",
			chartVersion, strings.TrimPrefix(chartVersion, "v"))
	}
	out := make([]string, 0, 1+len(sub))
	out = append(out, fmt.Sprintf("%s/kubeflow/trainer/trainer-controller-manager:%s", defaultImageRegistry, chartVersion))
	return append(out, sub...), nil
}

// TrainerChartVersion returns the Kubeflow Trainer chart version the `[deps]`
// phase pins (without the leading "v", the way Helm stores chart versions).
func TrainerChartVersion() string {
	return strings.TrimPrefix(kubeflowTrainerVersion, "v")
}

// imageKeys matches map keys that hold a container image reference in
// unstructured pod-spec fragments. Only exact matches are collected, so
// unrelated fields never contribute.
var imageKeys = map[string]bool{
	"image": true,
}

// walkImages recursively walks a decoded YAML object and collects values
// under image keys.
func walkImages(v any, add func(string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if imageKeys[k] {
				if s, ok := child.(string); ok {
					add(s)
				}
			}
			walkImages(child, add)
		}
	case []any:
		for _, child := range t {
			walkImages(child, add)
		}
	}
}

// workloadImagesFromSpec walks a rendered WorkflowSpec and collects every
// container image reference:
//
//   - the trainer image on the TrainJob spec,
//   - runtimePatches (typed: no container image can hide in them —
//     ContainerPatch has no image field — but they are walked anyway in case
//     upstream adds one),
//   - the unstructured dependencies, which carry the TrainingRuntime pod
//     templates with the nccl-tests / TCPXO images.
func workloadImagesFromSpec(spec *nvcrev1alpha1.WorkflowSpec, source string) ([]ImageRef, error) {
	var out []ImageRef
	seen := map[string]bool{}
	add := func(image string) {
		image = strings.TrimSpace(image)
		if image == "" || seen[image] {
			return
		}
		seen[image] = true
		out = append(out, ImageRef{Image: image, Source: source, Kind: "workload"})
	}

	if tj := spec.JobTemplate.Spec.Workload.TrainJob; tj != nil {
		if tj.Trainer != nil && tj.Trainer.Image != nil {
			add(*tj.Trainer.Image)
		}
		for _, patch := range tj.RuntimePatches {
			if patch.TrainingRuntimeSpec == nil {
				continue
			}
			// TrainingRuntimeSpec is a typed struct, and walkImages only
			// understands the generic decoded form, so round-trip it. Walking
			// the typed value directly matched no case and silently collected
			// nothing.
			raw, err := yaml.Marshal(patch.TrainingRuntimeSpec)
			if err != nil {
				return nil, fmt.Errorf("encode runtime patch of %s: %w", source, err)
			}
			var obj any
			if err := yaml.Unmarshal(raw, &obj); err != nil {
				return nil, fmt.Errorf("decode runtime patch of %s: %w", source, err)
			}
			walkImages(obj, add)
		}
	}
	for _, dep := range spec.Dependencies {
		// DependencySpec embeds runtime.RawExtension: freshly unmarshalled
		// specs keep the payload in .Raw (Object is only populated by the
		// API serializer), so decode Raw before walking.
		var obj any = dep.Object
		if obj == nil && len(dep.Raw) > 0 {
			if err := yaml.Unmarshal(dep.Raw, &obj); err != nil {
				return nil, fmt.Errorf("decode dependency of %s: %w", source, err)
			}
		}
		walkImages(obj, add)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Image < out[j].Image })
	return out, nil
}

// catalogImages builds every registered catalog entry across the matrix and
// returns the resolved workload images.
func catalogImages() ([]ImageRef, error) {
	pairs, err := imageMatrix()
	if err != nil {
		return nil, err
	}
	target := nvcrev1alpha1.TargetSpec{} // Build does not read the target for images.

	var out []ImageRef
	seen := map[string]bool{}
	add := func(refs []ImageRef) {
		for _, r := range refs {
			if seen[r.Image] {
				continue
			}
			seen[r.Image] = true
			out = append(out, r)
		}
	}

	for _, cat := range catalog.List() {
		entry := catalog.Lookup(cat.Domain, cat.Variant)
		if entry == nil {
			continue
		}
		built := 0
		var lastBuildErr error
		for _, pa := range pairs {
			spec, err := buildForImages(entry, target, pa)
			if err != nil {
				// Some (platform, architecture) combinations are invalid for
				// an entry (minGPUs, TP×PP divisibility). Those combinations
				// can never run, so their images are irrelevant; skip them.
				// An entry that builds for NO combination is a different
				// thing and is caught after the loop.
				lastBuildErr = err
				continue
			}
			built++
			// Resolve overrides exactly like `nvcrectl certification render`:
			// detect platform/GPU arch from the embedded node template, apply,
			// then read the images out of the resolved spec.
			nodes, err := render.LoadEmbeddedNodes(pa.Platform, pa.GPUArch)
			if err != nil {
				return nil, fmt.Errorf("load node template %s-%s: %w", pa.Platform, pa.GPUArch, err)
			}
			workflow := &nvcrev1alpha1.Workflow{Spec: *spec.DeepCopy()}
			if _, err := render.ResolveWorkflow(workflow, nodes); err != nil {
				return nil, fmt.Errorf("resolve overrides for %s/%s on %s/%s: %w",
					cat.Domain, cat.Variant, pa.Platform, pa.GPUArch, err)
			}
			refs, err := workloadImagesFromSpec(&workflow.Spec, fmt.Sprintf("catalog/%s/%s", cat.Domain, cat.Variant))
			if err != nil {
				return nil, fmt.Errorf("collect images for %s/%s: %w", cat.Domain, cat.Variant, err)
			}
			add(refs)
		}
		// An entry that built for no combination contributed no images, so
		// whatever it pulls is missing from the manifest. Today that is
		// invisible only because the entry happens to share an image with a
		// sibling; the moment it is bumped, the mirror silently loses it and
		// the failure surfaces after egress is cut. Fail here instead.
		if built == 0 {
			return nil, fmt.Errorf(
				"catalog entry %s/%s built for none of the %d platform/architecture combinations, "+
					"so it contributed no images: %w",
				cat.Domain, cat.Variant, len(pairs), lastBuildErr)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Image < out[j].Image })
	return out, nil
}

// controllerImageRef returns the controller image setup init installs for the
// given version.
func controllerImageRef(version string) ImageRef {
	return ImageRef{
		Image:  defaultImage(version),
		Source: "helm/cluster-readiness-engine values manager.image + nvcrectl setup init --image default",
		Kind:   "controller",
	}
}

// trainerImageRefs returns the images the Kubeflow Trainer chart at
// chartVersion renders with the values `nvcrectl setup init` passes.
func trainerImageRefs(chartVersion string) ([]ImageRef, error) {
	refs, err := trainerChartImages(chartVersion)
	if err != nil {
		return nil, err
	}
	out := make([]ImageRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, ImageRef{
			Image:  r,
			Source: "kubeflow-trainer chart " + strings.TrimPrefix(chartVersion, "v") + " (recorded render)",
			Kind:   "trainer",
		})
	}
	return out, nil
}

// chartRefs returns the OCI chart references an install pulls.
func chartRefs(version, trainerVersion string) []ChartRef {
	return []ChartRef{
		{
			Chart:   helmChartOCI,
			Version: version,
			Source:  "nvcrectl setup init [helm] phase / helm install",
		},
		{
			Chart:   trainerHelmChartOCI,
			Version: trainerVersion,
			Source:  "nvcrectl setup init [deps] phase",
		},
	}
}

// dockerfileImageLine matches a FROM instruction, capturing the base image
// reference (registry/repository:tag, optionally @sha256:... pinned).
var dockerfileImageLine = regexp.MustCompile(`^FROM\s+(\S+)`)

// dockerfileBuildImages parses the repo Dockerfile (embedded at compile time)
// and returns the base images the controller builds from. An air-gapped build
// host needs them mirrored even though they never run on the cluster; the
// digest form is preserved because the Dockerfile pins base images by digest.
func dockerfileBuildImages() ([]ImageRef, error) {
	f, err := dockerfileFS.Open("testdata/dockerfile")
	if err != nil {
		return nil, fmt.Errorf("open embedded Dockerfile: %w", err)
	}
	defer func() { _ = f.Close() }()

	var out []ImageRef
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := dockerfileImageLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		image := m[1]
		if seen[image] {
			continue
		}
		seen[image] = true
		out = append(out, ImageRef{Image: image, Source: "Dockerfile (controller build base)", Kind: "build"})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan embedded Dockerfile: %w", err)
	}
	return out, nil
}

// BuildImageManifest assembles the complete air-gap mirror manifest. version
// feeds the controller image tag and the NVCRE chart version the same way
// `nvcrectl setup init` resolves them; empty means a "<version>" placeholder
// so the manifest stays renderable on dev builds.
func BuildImageManifest(opts ImageManifestOptions) (*ImageManifest, error) {
	version := strings.TrimSpace(opts.Version)
	trainerVersion := strings.TrimSpace(opts.TrainerVersion)
	if trainerVersion == "" {
		trainerVersion = TrainerChartVersion()
	}
	if version == "" {
		version = "<version>"
	}

	catalogRefs, err := catalogImages()
	if err != nil {
		return nil, fmt.Errorf("derive catalog images: %w", err)
	}
	buildRefs, err := dockerfileBuildImages()
	if err != nil {
		return nil, fmt.Errorf("derive build images: %w", err)
	}
	trainerRefs, err := trainerImageRefs(trainerVersion)
	if err != nil {
		return nil, fmt.Errorf("derive trainer images: %w", err)
	}

	images := make([]ImageRef, 0, 1+len(trainerRefs)+len(catalogRefs)+len(buildRefs))
	images = append(images, controllerImageRef(version))
	images = append(images, trainerRefs...)
	images = append(images, catalogRefs...)
	images = append(images, buildRefs...)

	return &ImageManifest{
		Images: images,
		Charts: chartRefs(version, trainerVersion),
	}, nil
}
