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
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// localSSDBundled reports whether the named machine type bundles local SSDs
// (count is fixed by the machine type, not user config).
//
// Stub: Phase 2.3 fills in the real predicate; this returns false so that the
// conflict path is exercised by tests but not yet enforced in production.
func localSSDBundled(name string) bool {
	_ = name
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
// local SSDs (top-level count or legacy disk entry) on a machine type that
// already bundles them. The returned error is wrapped in *retryableError by
// the caller so the Create loop falls through to a compatible instance type.
//
// Stub: Phase 2.3 fills in the real logic.
func evaluateLocalSSDConflict(nodeClass *v1alpha1.GCENodeClass, instanceTypeName string) error {
	_ = nodeClass
	_ = instanceTypeName
	return nil
}
