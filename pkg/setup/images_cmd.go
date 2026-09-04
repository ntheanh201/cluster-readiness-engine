// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// `nvcrectl setup images` — emit the complete image manifest an operator can
// mirror to a private registry in one step (issue #243, air-gapped install).
package setup

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// newImagesCommand returns the "setup images" subcommand.
func newImagesCommand(version string) *cobra.Command {
	var outputFormat string
	var imageVersion string
	var trainerVersion string

	cmd := &cobra.Command{
		Use:   "images",
		Short: "Print every image an air-gapped install must mirror",
		Long: `Prints the complete image manifest for an air-gapped (offline) install.

The manifest is derived from the same sources the running system uses: the
controller image setup init installs, the Kubeflow Trainer stack pinned by
the [deps] phase, every catalog workload image resolved across all platform
and GPU-architecture overrides, the two OCI Helm charts, and the controller
Dockerfile build bases.

Mirror the printed images to a private registry before running setup init in
the air-gapped environment, then pass --image on init to point at the mirror:

  nvcrectl setup images --output yaml > images.yaml
  # ... mirror each image ...
  nvcrectl setup init --image <mirror>/cluster-readiness-engine/manager:<tag>

Workload images referenced by a Certification are pulled by the workload pods
themselves; mirror them too, or reference mirrored equivalents from the
Certification spec.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			v := imageVersion
			if v == "" {
				v = version
			}
			m, err := BuildImageManifest(ImageManifestOptions{
				Version:        v,
				TrainerVersion: trainerVersion,
			})
			if err != nil {
				return err
			}
			return printImageManifest(m, outputFormat)
		},
	}

	cmd.Flags().StringVar(&outputFormat, "output", "table", "Output format: table, yaml, or json")
	cmd.Flags().StringVar(&imageVersion, "image-version", "",
		"Controller image version when it differs from the CLI version (e.g. a release tag on a dev build)")
	cmd.Flags().StringVar(&trainerVersion, "trainer-version", "",
		"Kubeflow Trainer chart version when it differs from the pinned default")
	return cmd
}

// printImageManifest renders the manifest in the requested format.
func printImageManifest(m *ImageManifest, format string) error {
	switch strings.ToLower(format) {
	case "json":
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %w", err)
		}
		fmt.Println(string(data))
	case "yaml":
		data, err := yaml.Marshal(m)
		if err != nil {
			return fmt.Errorf("marshal yaml: %w", err)
		}
		fmt.Print(string(data))
	default: // table
		fmt.Printf("%-11s %s\n", "KIND", "IMAGE")
		for _, r := range m.Images {
			fmt.Printf("%-11s %s\n", r.Kind, r.Image)
		}
		fmt.Println()
		fmt.Println("Helm charts (OCI):")
		for _, c := range m.Charts {
			fmt.Printf("  %s:%s\n", c.Chart, c.Version)
		}
	}
	return nil
}
