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

// Keys patched by PatchLocalSSDMetadata. Centralized so tests and the helper
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
		envKey, envValue, envDrop string
		labelKey, labelDrop       string
	)
	switch mode {
	case v1alpha1.LocalSSDModeEphemeral:
		envKey, envValue = kubeEnvKeyLocalSSDsEphemeral, "true"
		envDrop = kubeEnvKeyLocalSSDsExt
		labelKey, labelDrop = kubeLabelKeyEphemeralLS, kubeLabelKeyLocalNVMe
	default:
		// RawBlock is the default; treat empty string as RawBlock so a
		// NodeClass that omits the field still gets sensible kube-env.
		envKey, envValue = kubeEnvKeyLocalSSDsExt, fmt.Sprintf("%d,nvme,block", count)
		envDrop = kubeEnvKeyLocalSSDsEphemeral
		labelKey, labelDrop = kubeLabelKeyLocalNVMe, kubeLabelKeyEphemeralLS
	}

	var sawEnv, sawLabels bool
	for _, it := range items {
		switch it.Key {
		case "kube-env":
			sawEnv = true
			v := lo.FromPtr(it.Value)
			v = removeKubeEnvKey(v, envDrop)
			v = setKubeEnvKey(v, envKey, envValue)
			it.Value = lo.ToPtr(v)
		case "kube-labels":
			sawLabels = true
			v := lo.FromPtr(it.Value)
			v = removeKubeLabel(v, labelDrop)
			v = setKubeLabel(v, labelKey, "true")
			it.Value = lo.ToPtr(v)
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

// setKubeEnvKey replaces any "key: ..." line in env with "key: value", or
// appends "key: value" if no such line exists. Preserves line order and the
// canonical trailing newline.
func setKubeEnvKey(env, key, value string) string {
	prefix := key + ":"
	newLine := key + ": " + value
	trimmed := strings.TrimRight(env, "\n")
	lines := strings.Split(trimmed, "\n")
	out := make([]string, 0, len(lines)+1)
	replaced := false
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			if !replaced {
				out = append(out, newLine)
				replaced = true
			}
			continue
		}
		out = append(out, line)
	}
	if !replaced {
		out = append(out, newLine)
	}
	return joinWithTrailingNewline(out, env)
}

// removeKubeEnvKey drops any "key: ..." line from env. No-op if absent.
// Preserves the canonical trailing newline.
func removeKubeEnvKey(env, key string) string {
	prefix := key + ":"
	trimmed := strings.TrimRight(env, "\n")
	lines := strings.Split(trimmed, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			continue
		}
		out = append(out, line)
	}
	return joinWithTrailingNewline(out, env)
}

func joinWithTrailingNewline(lines []string, original string) string {
	result := strings.Join(lines, "\n")
	if strings.HasSuffix(original, "\n") {
		result += "\n"
	}
	return result
}

// setKubeLabel ensures "key=value" appears exactly once in labels, replacing
// any existing "key=..." pair. Labels are comma-separated.
func setKubeLabel(labels, key, value string) string {
	parts := strings.Split(labels, ",")
	out := make([]string, 0, len(parts)+1)
	replaced := false
	for _, raw := range parts {
		pair := strings.TrimSpace(raw)
		if pair == "" {
			continue
		}
		k, _, _ := strings.Cut(pair, "=")
		if k == key {
			if !replaced {
				out = append(out, key+"="+value)
				replaced = true
			}
			continue
		}
		out = append(out, pair)
	}
	if !replaced {
		out = append(out, key+"="+value)
	}
	return strings.Join(out, ",")
}

// removeKubeLabel drops any "key=..." pair from labels. No-op if absent.
func removeKubeLabel(labels, key string) string {
	parts := strings.Split(labels, ",")
	out := make([]string, 0, len(parts))
	for _, raw := range parts {
		pair := strings.TrimSpace(raw)
		if pair == "" {
			continue
		}
		if k, _, _ := strings.Cut(pair, "="); k == key {
			continue
		}
		out = append(out, pair)
	}
	return strings.Join(out, ",")
}
