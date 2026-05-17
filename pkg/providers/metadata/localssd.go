/*
Copyright 2025 The CloudPilot AI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metadata

import (
	"fmt"
	"strings"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// Keys patched by PatchLocalSSDMetadata. Centralised so tests and the helper
// stay in sync.
const (
	// kube-env (YAML scalar) keys
	kubeEnvKeyLocalSSDsExt       = "NODE_LOCAL_SSDS_EXT"
	kubeEnvKeyLocalSSDsEphemeral = "NODE_LOCAL_SSDS_EPHEMERAL"

	// kube-labels (comma-separated key=value) keys
	kubeLabelKeyLocalNVMe   = "cloud.google.com/gke-local-nvme-ssd"
	kubeLabelKeyEphemeralLS = "cloud.google.com/gke-ephemeral-storage-local-ssd"
)

// PatchLocalSSDMetadata upserts the kube-env and kube-labels entries that GKE
// uses to tell the bootstrapper how to expose local SSDs.
//
//   - count == 0: no mutation (caller responsibility to skip when zero).
//   - mode == RawBlock: kube-env gets `NODE_LOCAL_SSDS_EXT: "<count>,nvme,block"`
//     and kube-labels gets `cloud.google.com/gke-local-nvme-ssd=true`.
//   - mode == Ephemeral: kube-env gets `NODE_LOCAL_SSDS_EPHEMERAL: "true"`
//     and kube-labels gets `cloud.google.com/gke-ephemeral-storage-local-ssd=true`.
//
// Stale entries for the opposite mode are removed so a mode flip on an
// existing kube-env produces the expected single-mode state.
//
// Returns an error if either kube-env or kube-labels is absent from items
// while count > 0, consistent with AppendGPUTaint and PatchKubeEnvForInstanceType.
func PatchLocalSSDMetadata(items []*compute.MetadataItems, mode v1alpha1.LocalSSDMode, count int) ([]*compute.MetadataItems, error) {
	if count <= 0 {
		return items, nil
	}

	var (
		envKeep, envDrop, envLine string
		labelKeep, labelDrop      string
	)
	switch mode {
	case v1alpha1.LocalSSDModeEphemeral:
		envKeep, envDrop = kubeEnvKeyLocalSSDsEphemeral, kubeEnvKeyLocalSSDsExt
		envLine = kubeEnvKeyLocalSSDsEphemeral + ": true"
		labelKeep, labelDrop = kubeLabelKeyEphemeralLS, kubeLabelKeyLocalNVMe
	default:
		// RawBlock is the default; treat empty string as RawBlock so a
		// NodeClass that omits the field still gets sensible kube-env.
		envKeep, envDrop = kubeEnvKeyLocalSSDsExt, kubeEnvKeyLocalSSDsEphemeral
		envLine = fmt.Sprintf("%s: %d,nvme,block", kubeEnvKeyLocalSSDsExt, count)
		labelKeep, labelDrop = kubeLabelKeyLocalNVMe, kubeLabelKeyEphemeralLS
	}

	var sawEnv, sawLabels bool
	for _, it := range items {
		switch it.Key {
		case "kube-env":
			sawEnv = true
			it.Value = lo.ToPtr(upsertKubeEnvLine(lo.FromPtr(it.Value), envKeep, envDrop, envLine))
		case "kube-labels":
			sawLabels = true
			it.Value = lo.ToPtr(upsertKubeLabel(lo.FromPtr(it.Value), labelKeep, labelDrop))
		}
	}
	if !sawEnv {
		return items, fmt.Errorf("kube-env metadata item not found")
	}
	if !sawLabels {
		return items, fmt.Errorf("kube-labels metadata item not found")
	}
	return items, nil
}

// upsertKubeEnvLine drops any line starting with "<dropKey>:" and replaces
// (or appends) the line starting with "<keepKey>:" with newLine.
func upsertKubeEnvLine(kubeEnv, keepKey, dropKey, newLine string) string {
	keepPrefix := keepKey + ":"
	dropPrefix := dropKey + ":"
	// Canonical kube-env ends with "\n"; strip it before Split so the trailing
	// empty element doesn't slot in front of an appended newLine.
	trimmed := strings.TrimRight(kubeEnv, "\n")
	lines := strings.Split(trimmed, "\n")
	out := make([]string, 0, len(lines)+1)
	replaced := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, dropPrefix):
			// drop stale opposite-mode line
		case strings.HasPrefix(line, keepPrefix):
			if !replaced {
				out = append(out, newLine)
				replaced = true
			}
			// extra duplicates (shouldn't happen) collapse to one
		default:
			out = append(out, line)
		}
	}
	if !replaced {
		out = append(out, newLine)
	}
	result := strings.Join(out, "\n")
	if strings.HasSuffix(kubeEnv, "\n") {
		result += "\n"
	}
	return result
}

// upsertKubeLabel removes any "<dropKey>=..." pair from a comma-separated
// label string and ensures "<keepKey>=true" appears exactly once.
func upsertKubeLabel(kubeLabels, keepKey, dropKey string) string {
	parts := strings.Split(kubeLabels, ",")
	out := make([]string, 0, len(parts)+1)
	keepFound := false
	for _, raw := range parts {
		pair := strings.TrimSpace(raw)
		if pair == "" {
			continue
		}
		k, _, _ := strings.Cut(pair, "=")
		switch k {
		case dropKey:
			// drop
		case keepKey:
			if keepFound {
				continue
			}
			keepFound = true
			out = append(out, keepKey+"=true")
		default:
			out = append(out, pair)
		}
	}
	if !keepFound {
		out = append(out, keepKey+"=true")
	}
	return strings.Join(out, ",")
}
