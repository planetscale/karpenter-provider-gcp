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
// Stub: Phase 2.5 fills in the body.
func PatchLocalSSDMetadata(items []*compute.MetadataItems, mode v1alpha1.LocalSSDMode, count int) []*compute.MetadataItems {
	_ = mode
	_ = count
	return items
}
