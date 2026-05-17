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
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func mkItems(kubeEnv, kubeLabels string) []*compute.MetadataItems {
	return []*compute.MetadataItems{
		{Key: "kube-env", Value: lo.ToPtr(kubeEnv)},
		{Key: "kube-labels", Value: lo.ToPtr(kubeLabels)},
	}
}

func kubeEnvLine(items []*compute.MetadataItems, key string) (string, bool) {
	for _, it := range items {
		if it.Key != "kube-env" {
			continue
		}
		for _, line := range strings.Split(lo.FromPtr(it.Value), "\n") {
			if strings.HasPrefix(line, key+":") {
				return line, true
			}
		}
	}
	return "", false
}

func kubeLabelValue(items []*compute.MetadataItems, key string) (string, bool) {
	for _, it := range items {
		if it.Key != "kube-labels" {
			continue
		}
		for _, pair := range strings.Split(lo.FromPtr(it.Value), ",") {
			pair = strings.TrimSpace(pair)
			if k, v, ok := strings.Cut(pair, "="); ok && k == key {
				return v, true
			}
		}
	}
	return "", false
}

func TestPatchLocalSSDMetadata_NoMutationWhenCountZero(t *testing.T) {
	t.Parallel()
	before := mkItems("KUBELET_ARGS: --foo=bar\n", "a=b,c=d")
	bv := lo.FromPtr(before[0].Value)
	bl := lo.FromPtr(before[1].Value)

	after, err := PatchLocalSSDMetadata(before, v1alpha1.LocalSSDModeRawBlock, 0)
	require.NoError(t, err)

	require.Len(t, after, 2)
	assert.Equal(t, bv, lo.FromPtr(after[0].Value), "kube-env must not change")
	assert.Equal(t, bl, lo.FromPtr(after[1].Value), "kube-labels must not change")
}

func TestPatchLocalSSDMetadata_RawBlock(t *testing.T) {
	t.Parallel()
	items := mkItems("KUBELET_ARGS: --foo=bar\n", "a=b")

	got, err := PatchLocalSSDMetadata(items, v1alpha1.LocalSSDModeRawBlock, 2)
	require.NoError(t, err)

	line, ok := kubeEnvLine(got, "NODE_LOCAL_SSDS_EXT")
	require.True(t, ok, "expected NODE_LOCAL_SSDS_EXT line in kube-env")
	assert.Contains(t, line, "2,nvme,block")

	val, ok := kubeLabelValue(got, "cloud.google.com/gke-local-nvme-ssd")
	require.True(t, ok, "expected gke-local-nvme-ssd label")
	assert.Equal(t, "true", val)

	_, ephOK := kubeEnvLine(got, "NODE_LOCAL_SSDS_EPHEMERAL")
	assert.False(t, ephOK, "ephemeral key must not appear in RawBlock mode")
}

func TestPatchLocalSSDMetadata_Ephemeral(t *testing.T) {
	t.Parallel()
	items := mkItems("KUBELET_ARGS: --foo=bar\n", "a=b")

	got, err := PatchLocalSSDMetadata(items, v1alpha1.LocalSSDModeEphemeral, 1)
	require.NoError(t, err)

	line, ok := kubeEnvLine(got, "NODE_LOCAL_SSDS_EPHEMERAL")
	require.True(t, ok, "expected NODE_LOCAL_SSDS_EPHEMERAL line in kube-env")
	assert.Contains(t, line, "true")

	val, ok := kubeLabelValue(got, "cloud.google.com/gke-ephemeral-storage-local-ssd")
	require.True(t, ok, "expected ephemeral-storage-local-ssd label")
	assert.Equal(t, "true", val)

	_, rawOK := kubeEnvLine(got, "NODE_LOCAL_SSDS_EXT")
	assert.False(t, rawOK, "raw-block key must not appear in Ephemeral mode")
}

func TestPatchLocalSSDMetadata_UpsertReplacesExisting(t *testing.T) {
	t.Parallel()
	// kube-env already has a stale NODE_LOCAL_SSDS_EXT line; helper must
	// replace (not duplicate) it.
	kubeEnv := "KUBELET_ARGS: --foo=bar\nNODE_LOCAL_SSDS_EXT: 9,nvme,block\n"
	kubeLabels := "a=b,cloud.google.com/gke-local-nvme-ssd=true"
	items := mkItems(kubeEnv, kubeLabels)

	got, err := PatchLocalSSDMetadata(items, v1alpha1.LocalSSDModeRawBlock, 4)
	require.NoError(t, err)

	// Only one NODE_LOCAL_SSDS_EXT line, with the new count.
	value := lo.FromPtr(got[0].Value)
	occurrences := strings.Count(value, "NODE_LOCAL_SSDS_EXT")
	assert.Equal(t, 1, occurrences, "exactly one NODE_LOCAL_SSDS_EXT line expected")
	line, _ := kubeEnvLine(got, "NODE_LOCAL_SSDS_EXT")
	assert.Contains(t, line, "4,nvme,block")

	// kube-labels still has exactly one nvme-ssd label.
	lblValue := lo.FromPtr(got[1].Value)
	assert.Equal(t, 1, strings.Count(lblValue, "cloud.google.com/gke-local-nvme-ssd"),
		"label must not be duplicated")
}

func TestPatchLocalSSDMetadata_ErrorWhenKubeEnvMissing(t *testing.T) {
	t.Parallel()
	items := []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("a=b")},
	}
	_, err := PatchLocalSSDMetadata(items, v1alpha1.LocalSSDModeRawBlock, 2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-env")
}

func TestPatchLocalSSDMetadata_ErrorWhenKubeLabelsMissing(t *testing.T) {
	t.Parallel()
	items := []*compute.MetadataItems{
		{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --foo=bar\n")},
	}
	_, err := PatchLocalSSDMetadata(items, v1alpha1.LocalSSDModeRawBlock, 2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kube-labels")
}

func TestPatchLocalSSDMetadata_AppendPreservesCanonicalShape(t *testing.T) {
	t.Parallel()
	// Canonical kube-env ends in "\n" and has no blank lines. Appending a
	// new key must not introduce a blank line before the new entry and must
	// preserve the trailing newline.
	kubeEnv := "KUBELET_ARGS: --foo=bar\nDNS_SERVER_IP: 10.0.0.10\n"
	items := mkItems(kubeEnv, "a=b")

	got, err := PatchLocalSSDMetadata(items, v1alpha1.LocalSSDModeRawBlock, 2)
	require.NoError(t, err)
	envValue := lo.FromPtr(got[0].Value)

	assert.NotContains(t, envValue, "\n\n", "no blank lines in kube-env output")
	assert.True(t, strings.HasSuffix(envValue, "\n"), "trailing newline preserved")
	assert.Equal(t,
		"KUBELET_ARGS: --foo=bar\nDNS_SERVER_IP: 10.0.0.10\nNODE_LOCAL_SSDS_EXT: 2,nvme,block\n",
		envValue)
}

func TestPatchLocalSSDMetadata_ModeFlipRemovesOldKeys(t *testing.T) {
	t.Parallel()
	// Start in Ephemeral state; switch to RawBlock.
	kubeEnv := "NODE_LOCAL_SSDS_EPHEMERAL: true\nKUBELET_ARGS: --foo=bar\n"
	kubeLabels := "a=b,cloud.google.com/gke-ephemeral-storage-local-ssd=true"
	items := mkItems(kubeEnv, kubeLabels)

	got, err := PatchLocalSSDMetadata(items, v1alpha1.LocalSSDModeRawBlock, 2)
	require.NoError(t, err)

	envValue := lo.FromPtr(got[0].Value)
	assert.NotContains(t, envValue, "NODE_LOCAL_SSDS_EPHEMERAL",
		"ephemeral kube-env line must be removed on flip to RawBlock")
	assert.Contains(t, envValue, "NODE_LOCAL_SSDS_EXT")

	lblValue := lo.FromPtr(got[1].Value)
	assert.NotContains(t, lblValue, "gke-ephemeral-storage-local-ssd",
		"ephemeral label must be removed on flip to RawBlock")
	assert.Contains(t, lblValue, "gke-local-nvme-ssd=true")
}
