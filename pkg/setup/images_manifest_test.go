// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// ---------------------------------------------------------------------------
// Golden test: the manifest shape itself.
// ---------------------------------------------------------------------------

// TestImageManifestGolden pins the generated manifest. When a catalog entry,
// the trainer pin, or the Dockerfile changes, the manifest changes with it:
// review the diff, confirm it is intended, then regenerate with
// TESTUTIL_UPDATE_EXPECTED=true go test ./pkg/setup/ -run TestImageManifestGolden.
func TestImageManifestGolden(t *testing.T) {
	m, err := BuildImageManifest(ImageManifestOptions{Version: "v1.0.0-test"})
	require.NoError(t, err)

	data, err := yaml.Marshal(m)
	require.NoError(t, err)

	parser := testutil.TestCaseParser{
		Subdir:         "image-manifest",
		ExpectedSuffix: ".yaml",
	}
	// TestDir expects a testdata/<Subdir>/<case>/ layout; use a single case.
	parser.TestDir(t, func(tc *testutil.TestCase) error {
		tc.Actual = string(data)
		return nil
	})
}

// imageAssignPattern matches a line that assigns a container image: the
// `image:` / `repository:` YAML keys (with or without a leading "- ") or a
// Dockerfile FROM instruction. Matching assignment lines only — not every
// URL-shaped string — keeps annotations, API groups, and doc links out of
// the scan while still catching every way an image enters the repo.
var imageAssignPattern = regexp.MustCompile(
	`^\s*(?:-\s+)?(?:image|repository):\s*(\S+)|^FROM\s+(\S+)`)

// imageRefPattern matches a fully-qualified container image reference:
// registry (with a dot or localhost or a port), repository, optional tag or
// digest. Anchored — bare names like "dra-stub:local" (test harness images
// built and loaded locally, never mirrored) and Helm template expressions do
// not match.
var imageRefPattern = regexp.MustCompile(
	`^(?:[a-zA-Z0-9][a-zA-Z0-9-]*(?:\.[a-zA-Z0-9][a-zA-Z0-9-]*)+(?::[0-9]+)?|localhost(?::[0-9]+)?)` +
		`/[a-zA-Z0-9][a-zA-Z0-9_/.-]*` +
		`(?:@sha256:[a-f0-9]{64})?$`)

// scannedSourceDirs are the repo sources a new image reference can appear in:
// catalog entries and their shared libraries, the platform override templates
// (which reuse the same _lib fragments), the chart values, and the Dockerfile.
// Test-only harnesses (test/uat builds its own local images) and generated
// CRD schemas (which describe volume sources, not images) are excluded.
var scannedSourceDirs = []string{
	"../../pkg/catalog/entries",
	"../../pkg/platform/overrides",
	"../../helm/cluster-readiness-engine",
}

var scannedRootFiles = []string{
	"../../Dockerfile",
}

// repoImageReferences scans the scanned source files and returns every
// fully-qualified image reference assigned on an image/repository/FROM line,
// with the file it appears in. Comment lines are skipped so prose that quotes
// an image is not flagged.
func repoImageReferences(t *testing.T) map[string]string {
	t.Helper()
	refs := map[string]string{}

	scanFile := func(path string) {
		data, err := os.ReadFile(path) // #nosec G304 -- test-controlled repo paths
		require.NoError(t, err, "reading %s", path)
		for lineNo, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			m := imageAssignPattern.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			value := m[1]
			if value == "" {
				value = m[2] // Dockerfile FROM
			}
			value = strings.Trim(value, `"'`)
			if !imageRefPattern.MatchString(value) {
				// Tagless repository (the chart values form) or a template
				// expression; keep the repository so the tagless form is
				// still covered, drop expressions.
				if strings.Contains(value, "{{") {
					continue
				}
				if _, ok := splitRepository(value); !ok {
					continue
				}
			}
			// Record the first file each reference appears in.
			if _, ok := refs[value]; !ok {
				refs[value] = fmt.Sprintf("%s:%d", path, lineNo+1)
			}
		}
	}

	for _, dir := range scannedSourceDirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			switch filepath.Ext(path) {
			case ".yaml", ".yml", ".tpl":
				scanFile(path)
			}
			return nil
		})
		require.NoError(t, err, "walking %s", dir)
	}
	for _, f := range scannedRootFiles {
		scanFile(f)
	}
	return refs
}

// splitRepository returns the repository part of an image reference,
// stripping any tag or digest so a tagless values.yaml reference and the
// versioned manifest entry compare equal. ok is false for anything that is
// not a registry-qualified reference.
func splitRepository(ref string) (repo string, ok bool) {
	slash := strings.Index(ref, "/")
	if slash <= 0 {
		return "", false
	}
	registry, repo := ref[:slash], ref[slash+1:]
	if !strings.Contains(registry, ".") && registry != "localhost" {
		return "", false
	}
	if at := strings.Index(repo, "@"); at >= 0 {
		repo = repo[:at]
	}
	if colon := strings.LastIndex(repo, ":"); colon >= 0 {
		repo = repo[:colon]
	}
	return repo, true
}

// TestManifestCoversRepoImageReferences is the honesty check for the air-gap
// manifest: when a new image is added anywhere under the scanned sources and
// the manifest does not pick it up, this fails. Fix by extending the
// derivation (a new catalog entry or a new image field) — never by adding the
// image to a hardcoded list here.
func TestManifestCoversRepoImageReferences(t *testing.T) {
	m, err := BuildImageManifest(ImageManifestOptions{Version: "v1.0.0-test"})
	require.NoError(t, err)

	// Two indexes on purpose. A scanned reference that names a tag or digest
	// must be mirrored at that exact reference: matching on the repository
	// alone would let a new tag of an already-listed repository pass while the
	// mirror lacks it, which is precisely how nemotron5-56b's images went
	// missing without this test noticing. Tagless references (the chart values
	// form) can only be checked by repository.
	manifestRefs := make(map[string]bool, len(m.Images))
	manifestRepos := make(map[string]bool, len(m.Images))
	for _, r := range m.Images {
		manifestRefs[r.Image] = true
		repo, ok := splitRepository(r.Image)
		require.True(t, ok, "manifest image %q is not registry-qualified", r.Image)
		manifestRepos[repo] = true
	}

	// The embedded Dockerfile snapshot must match the real repo Dockerfile,
	// otherwise build-base images drift out of the manifest.
	embedded, err := dockerfileFS.ReadFile("testdata/dockerfile")
	require.NoError(t, err)
	repoDockerfile, err := os.ReadFile("../../Dockerfile")
	require.NoError(t, err)
	assert.Equal(t, fromLines(repoDockerfile), fromLines(embedded),
		"pkg/setup/testdata/dockerfile is stale relative to the repo Dockerfile; copy the FROM lines over")

	for image, where := range repoImageReferences(t) {
		repo, ok := splitRepository(image)
		require.True(t, ok, "scanned image %q (from %s) is not registry-qualified", image, where)
		if pinnedReference(image) {
			assert.Contains(t, manifestRefs, image,
				"%s references image %q but the air-gap manifest does not list that exact reference; extend the derivation in images_manifest.go so a new image or tag cannot ship unmirrored", where, image)
			continue
		}
		assert.Contains(t, manifestRepos, repo,
			"%s references repository %q but the air-gap manifest does not list it; extend the derivation in images_manifest.go so a new image cannot ship unmirrored", where, image)
	}
}

// pinnedReference reports whether ref names a specific tag or digest, as
// opposed to the bare repository a chart values file carries.
func pinnedReference(ref string) bool {
	if strings.Contains(ref, "@") {
		return true
	}
	slash := strings.LastIndex(ref, "/")
	return strings.Contains(ref[slash+1:], ":")
}

// fromLines extracts the FROM image references from a Dockerfile body.
func fromLines(data []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if m := dockerfileImageLine.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Trainer chart images: pinned against the real chart.
// ---------------------------------------------------------------------------

// TestUnknownTrainerVersionFails pins the behaviour that matters more than the
// golden: a trainer version with no recorded render must be an error, not a
// guess. Sub-chart tags move independently of the chart version — jobset went
// v0.11.0 -> v0.12.0 between trainer 2.2.1 and 2.3.0 — so emitting the tags of
// the last known version would send an operator to mirror the wrong images and
// find out only after egress is cut.
func TestUnknownTrainerVersionFails(t *testing.T) {
	_, err := trainerChartImages("v99.99.99")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no recorded sub-chart images")
	require.Contains(t, err.Error(), "helm template", "the error must say how to fix it")

	_, err = BuildImageManifest(ImageManifestOptions{TrainerVersion: "99.99.99"})
	require.Error(t, err, "an unknown trainer version must fail the whole manifest")
}

// TestEveryCatalogEntryContributesImages guards the failure the sweep used to
// hide. Building each entry at a single fixed node count silently produced
// nothing for training/nemotron5-56b, whose parallelism needs 32 GPUs: it was
// skipped for all 13 platform/architecture combinations, and the omission was
// invisible only because it happens to share an image with nemotron5-8b. Bump
// that entry's image and the air-gap mirror loses it without a word.
func TestEveryCatalogEntryContributesImages(t *testing.T) {
	pairs, err := imageMatrix()
	require.NoError(t, err)

	for _, cat := range catalog.List() {
		entry := catalog.Lookup(cat.Domain, cat.Variant)
		if entry == nil {
			continue
		}
		built := 0
		for _, pa := range pairs {
			if _, err := buildForImages(entry, nvcrev1alpha1.TargetSpec{}, pa); err == nil {
				built++
			}
		}
		require.NotZero(t, built,
			"catalog entry %s/%s built for none of the %d platform/architecture combinations, "+
				"so it contributes no images to the air-gap manifest",
			cat.Domain, cat.Variant, len(pairs))
	}
}

// TestTrainerChartImagesGolden pins the Kubeflow Trainer image list against
// the images the real chart at the pinned version renders with setup init's
// exact helm args. When the pin in setup.go moves, this test fails: do NOT
// regenerate the golden. Render the chart at the new version and add its
// sub-chart images to trainerSubchartImages first — regenerating alone would
// keep the previous sub-chart tags, which is the bug the table exists to stop.
//
//	helm template kubeflow-trainer oci://ghcr.io/kubeflow/charts/kubeflow-trainer \
//	  --version <v> --set manager.tolerations[0].operator=Exists \
//	  --set jobset.controller.tolerations[0].operator=Exists \
//	  | grep -E '^\s*image:' | sort -u
func TestTrainerChartImagesGolden(t *testing.T) {
	got, err := trainerChartImages(TrainerChartVersion())
	require.NoError(t, err)
	slices.Sort(got)

	data, err := yaml.Marshal(map[string]any{"images": got})
	require.NoError(t, err)

	parser := testutil.TestCaseParser{
		Subdir:         "trainer-chart-images",
		ExpectedSuffix: ".yaml",
	}
	parser.TestDir(t, func(tc *testutil.TestCase) error {
		tc.Actual = string(data)
		return nil
	})
}
