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

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// localSSDBundled reports whether the named machine type bundles local SSDs
// (count is fixed by the machine type, not user config).
//
// Suffix → families (verified 2026-05-17, see docs/claude/lssd-machine-type-audit.md):
//
//	-lssd            C3, C3D, C4 VM, C4A, C4D, H4D
//	-lssd-metal      C4 bare metal
//	-standardlssd    Z3 only
//	-highlssd        Z3 only
//	-highlssd-metal  Z3 bare metal only
//
// Every Z3 SKU that ships local SSDs uses one of the three Z3-specific
// suffixes; there is no bare "z3-*" SKU that bundles SSDs.
func localSSDBundled(name string) bool {
	return strings.HasSuffix(name, "-lssd") ||
		strings.HasSuffix(name, "-lssd-metal") ||
		strings.HasSuffix(name, "-standardlssd") ||
		strings.HasSuffix(name, "-highlssd") ||
		strings.HasSuffix(name, "-highlssd-metal")
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
func evaluateLocalSSDConflict(nodeClass *v1alpha1.GCENodeClass, instanceTypeName string) error {
	if nodeClass == nil || !localSSDBundled(instanceTypeName) {
		return nil
	}
	if nodeClass.Spec.LocalSsdCount == 0 && !hasLegacyLocalSSDDisk(nodeClass) {
		return nil
	}
	return fmt.Errorf(
		"machine type %s bundles local SSDs; remove spec.localSsdCount and any disks[].category=local-ssd entries (use spec.localSsdMode to control exposure)",
		instanceTypeName)
}
