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

package instance

import (
	"fmt"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// hasBundledLocalSSDs reports whether the named machine type bundles local SSDs
// (count is fixed by the machine type, not user config).
//
// The API field `MachineType.BundledLocalSsds.PartitionCount` is the
// authoritative signal and is preferred when mt is non-nil. The name-based
// fallback is used only when the instance-type cache is empty (e.g. partial
// refresh, or a code path that doesn't have the MachineType in scope).
//
// Name-based fallback families:
//
//	suffix -lssd / -lssd-metal              C3, C3D, C4 VM, C4A, C4D, H4D, C4 metal
//	suffix -standardlssd / -highlssd        Z3 (incl. -highlssd-metal)
//	prefix a2-ultragpu-                     A2 ultra (a2-standard is NOT bundled)
//	prefix a3- / a4- / a4x-                 all current accelerator SKUs except -nolssd siblings
//
// `-nolssd` siblings (e.g. a3-ultragpu-8g-nolssd, a3-edgegpu-8g-nolssd) are
// explicitly excluded by suffix.
func hasBundledLocalSSDs(name string, mt *computepb.MachineType) bool {
	if mt != nil {
		if bls := mt.GetBundledLocalSsds(); bls != nil && bls.PartitionCount != nil && *bls.PartitionCount > 0 {
			return true
		}
		// Cache hit with no bundled SSDs reported is authoritative: not bundled.
		return false
	}
	if strings.HasSuffix(name, "-nolssd") {
		return false
	}
	if strings.HasSuffix(name, "-lssd") ||
		strings.HasSuffix(name, "-lssd-metal") ||
		strings.HasSuffix(name, "-standardlssd") ||
		strings.HasSuffix(name, "-highlssd") ||
		strings.HasSuffix(name, "-highlssd-metal") {
		return true
	}
	return strings.HasPrefix(name, "a2-ultragpu-") ||
		strings.HasPrefix(name, "a3-") ||
		strings.HasPrefix(name, "a4-") ||
		strings.HasPrefix(name, "a4x-")
}

// hasLegacyLocalSSDDisk reports whether the NodeClass declares any
// `category: local-ssd` entry in spec.disks (the deprecated shape).
func hasLegacyLocalSSDDisk(nodeClass *v1alpha1.GCENodeClass) bool {
	if nodeClass == nil {
		return false
	}
	for _, d := range nodeClass.Spec.Disks {
		if d.Category == "local-ssd" {
			return true
		}
	}
	return false
}

// evaluateLocalSSDConflict returns a non-nil error if the NodeClass declares
// local SSDs (top-level count or legacy disk entry) on a machine type that
// already bundles them. The returned error is wrapped in *retryableError by
// the caller so the Create loop falls through to a compatible instance type
// rather than producing a node with more SCRATCH disks than the SKU allows.
//
// Pass mt from the instance-type cache when available; the helper prefers the
// API signal over name-based matching.
func evaluateLocalSSDConflict(nodeClass *v1alpha1.GCENodeClass, instanceTypeName string, mt *computepb.MachineType) error {
	if nodeClass == nil || !hasBundledLocalSSDs(instanceTypeName, mt) {
		return nil
	}
	if nodeClass.Spec.LocalSsdCount == 0 && !hasLegacyLocalSSDDisk(nodeClass) {
		return nil
	}
	return fmt.Errorf(
		"machine type %s bundles local SSDs; remove spec.localSsdCount and any disks[].category=local-ssd entries (use spec.localSsdMode to control exposure)",
		instanceTypeName)
}
