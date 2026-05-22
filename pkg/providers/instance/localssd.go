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

// Name-based fallback markers for hasBundledLocalSSDs. `-nolssd` siblings
// (e.g. a3-ultragpu-8g-nolssd) are explicitly excluded before these are
// considered, so we don't list a negative form here.
var (
	bundledLocalSSDSuffixes = []string{
		"-lssd",           // C3, C3D, C4 VM, C4A, C4D, H4D
		"-lssd-metal",     // C4 metal
		"-standardlssd",   // Z3
		"-highlssd",       // Z3
		"-highlssd-metal", // Z3 metal
	}
	bundledLocalSSDPrefixes = []string{
		"a2-ultragpu-", // A2 ultra (a2-standard is NOT bundled)
		"a3-",          // all current A3 SKUs (excluding -nolssd siblings)
		"a4-",
		"a4x-",
	}
)

// hasBundledLocalSSDs reports whether the named machine type bundles local SSDs
// (count is fixed by the machine type, not user config).
//
// The API field `MachineType.BundledLocalSsds.PartitionCount` is the
// authoritative signal and is preferred when mt is non-nil. The name-based
// fallback (the suffix/prefix tables above) is used only when the
// instance-type cache is empty (e.g. partial refresh, or a code path that
// doesn't have the MachineType in scope).
func hasBundledLocalSSDs(name string, mt *computepb.MachineType) bool {
	if mt != nil {
		bls := mt.GetBundledLocalSsds()
		return bls != nil && bls.PartitionCount != nil && *bls.PartitionCount > 0
	}
	if strings.HasSuffix(name, "-nolssd") {
		return false
	}
	for _, s := range bundledLocalSSDSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	for _, p := range bundledLocalSSDPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
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
// legacy disks[].category=local-ssd entries on a machine type that already
// bundles local SSDs. The returned error is wrapped in *retryableError by the
// caller so the Create loop falls through to a compatible instance type
// rather than producing a node with more SCRATCH disks than the SKU allows.
//
// Note: spec.localSsdCount is no longer checked here — it has been
// soft-deprecated and is silently ignored across the controller. Pin the
// count via the karpenter.k8s.gcp/instance-local-ssd-count label instead.
//
// Pass mt from the instance-type cache when available; the helper prefers the
// API signal over name-based matching.
func evaluateLocalSSDConflict(nodeClass *v1alpha1.GCENodeClass, instanceTypeName string, mt *computepb.MachineType) error {
	if nodeClass == nil || !hasBundledLocalSSDs(instanceTypeName, mt) {
		return nil
	}
	if !hasLegacyLocalSSDDisk(nodeClass) {
		return nil
	}
	return fmt.Errorf(
		"machine type %s bundles local SSDs; remove disks[].category=local-ssd entries (use spec.localSsdMode to control exposure)",
		instanceTypeName)
}
