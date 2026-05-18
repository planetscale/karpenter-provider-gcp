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
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// machineTypeWithBundledSSDs returns a *computepb.MachineType reporting `count` bundled
// local SSDs. count == 0 returns nil BundledLocalSsds, which is how the GCE
// API represents a non-bundled SKU.
func machineTypeWithBundledSSDs(count int32) *computepb.MachineType {
	if count == 0 {
		return &computepb.MachineType{}
	}
	return &computepb.MachineType{
		BundledLocalSsds: &computepb.BundledLocalSsds{
			PartitionCount: lo.ToPtr(count),
		},
	}
}

// TestHasBundledLocalSSDs_NameFallback exercises the name-based fallback used
// when the instance-type cache has no entry (mt == nil).
func TestHasBundledLocalSSDs_NameFallback(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// -lssd suffix (C3, C3D, C4 VM, C4A, C4D, H4D)
		{"c3-standard-88-lssd", true},
		{"c3d-highmem-360-lssd", true},
		{"c4-standard-32-lssd", true},
		{"c4a-standard-72-lssd", true},
		{"c4d-standard-8-lssd", true},
		{"c4d-standard-96-lssd", true},
		{"h4d-highmem-192-lssd", true},

		// -lssd-metal suffix (C4 bare metal)
		{"c4-standard-288-lssd-metal", true},
		{"c4-highmem-288-lssd-metal", true},

		// -standardlssd suffix (Z3 only)
		{"z3-highmem-14-standardlssd", true},
		{"z3-highmem-22-standardlssd", true},
		{"z3-highmem-88-standardlssd", true},

		// -highlssd suffix (Z3 only)
		{"z3-highmem-8-highlssd", true},
		{"z3-highmem-22-highlssd", true},
		{"z3-highmem-88-highlssd", true},

		// -highlssd-metal suffix (Z3 bare metal only)
		{"z3-highmem-192-highlssd-metal", true},

		// Accelerator families (a2-ultragpu-, a3-, a4-, a4x- prefixes)
		{"a2-ultragpu-1g", true},
		{"a2-ultragpu-2g", true},
		{"a2-ultragpu-4g", true},
		{"a2-ultragpu-8g", true},
		{"a3-ultragpu-8g", true},
		{"a3-megagpu-8g", true},
		{"a3-highgpu-1g", true},
		{"a3-highgpu-2g", true},
		{"a3-highgpu-4g", true},
		{"a3-highgpu-8g", true},
		{"a3-edgegpu-8g", true},
		{"a4-highgpu-8g", true},
		{"a4x-highgpu-4g", true},

		// -nolssd opt-out siblings — must NOT be flagged as bundled
		{"a3-ultragpu-8g-nolssd", false},
		{"a3-edgegpu-8g-nolssd", false},

		// a2-standard / a2-highgpu / a2-megagpu — manual-attach only
		{"a2-highgpu-1g", false},
		{"a2-highgpu-8g", false},
		{"a2-megagpu-16g", false},

		// non-bundled families
		{"n2-standard-8", false},
		{"n2d-standard-8", false},
		{"c2-standard-4", false},
		{"m1-ultramem-40", false},
		{"e2-medium", false},

		// z3 without an SSD suffix is not a real SKU; predicate must not
		// guess that bare "z3-*" bundles SSDs.
		{"z3-highmem-22", false},
		{"z3-highmem-88", false},

		// near-miss: "ssd" anywhere in the name shouldn't trigger
		{"some-ssd-family", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasBundledLocalSSDs(tc.name, nil))
		})
	}
}

// TestHasBundledLocalSSDs_APIPreferred asserts the API signal beats the name
// in either direction: a -nolssd opt-out is correctly reported as not
// bundled, and a SKU whose name suggests no SSDs but whose API field shows
// bundled count is still flagged as bundled.
func TestHasBundledLocalSSDs_APIPreferred(t *testing.T) {
	t.Run("API says bundled, name says no", func(t *testing.T) {
		// hypothetical future SKU without any known suffix/prefix
		assert.True(t, hasBundledLocalSSDs("future-family-fancy-1", machineTypeWithBundledSSDs(4)))
	})
	t.Run("API says not bundled, -nolssd sibling", func(t *testing.T) {
		assert.False(t, hasBundledLocalSSDs("a3-ultragpu-8g-nolssd", machineTypeWithBundledSSDs(0)))
	})
	t.Run("API authoritative over a3- prefix", func(t *testing.T) {
		// If a hypothetical a3- variant ever lacks bundled SSDs, the API
		// must win over the prefix fallback.
		assert.False(t, hasBundledLocalSSDs("a3-some-future-variant", machineTypeWithBundledSSDs(0)))
	})
}

func TestHasLegacyLocalSSDDisk(t *testing.T) {
	t.Run("no disks", func(t *testing.T) {
		assert.False(t, hasLegacyLocalSSDDisk(&v1alpha1.GCENodeClass{}))
	})
	t.Run("only boot disk", func(t *testing.T) {
		nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
			Disks: []v1alpha1.Disk{{Boot: true, SizeGiB: 50, Category: "pd-balanced"}},
		}}
		assert.False(t, hasLegacyLocalSSDDisk(nc))
	})
	t.Run("with legacy local-ssd entry", func(t *testing.T) {
		nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
			Disks: []v1alpha1.Disk{
				{Boot: true, SizeGiB: 50, Category: "pd-balanced"},
				{Category: "local-ssd"},
			},
		}}
		assert.True(t, hasLegacyLocalSSDDisk(nc))
	})
}

// TestEvaluateLocalSSDConflict asserts that when a bundled-SSD machine type is
// paired with user-declared local SSDs (top-level or legacy), the conflict
// helper returns a non-nil error. tryCreateInstance wraps this in
// *retryableError so the Create loop tries a different instance type rather
// than failing the NodeClaim outright.
func TestEvaluateLocalSSDConflict(t *testing.T) {
	cases := []struct {
		name     string
		nc       *v1alpha1.GCENodeClass
		mtName   string
		mt       *computepb.MachineType
		wantConf bool
	}{
		{
			name:     "bundled + top-level count → conflict",
			nc:       &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 1}},
			mtName:   "c4d-standard-8-lssd",
			mt:       machineTypeWithBundledSSDs(2),
			wantConf: true,
		},
		{
			name: "bundled + legacy disk-entry → conflict",
			nc: &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
				Disks: []v1alpha1.Disk{{Category: "local-ssd"}},
			}},
			mtName:   "z3-highmem-22-standardlssd",
			mt:       machineTypeWithBundledSSDs(12),
			wantConf: true,
		},
		{
			name:     "bundled z3 highlssd + top-level count → conflict",
			nc:       &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 1}},
			mtName:   "z3-highmem-22-highlssd",
			mt:       machineTypeWithBundledSSDs(24),
			wantConf: true,
		},
		{
			name: "bundled c4 metal + legacy disk-entry → conflict",
			nc: &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
				Disks: []v1alpha1.Disk{{Category: "local-ssd"}},
			}},
			mtName:   "c4-standard-288-lssd-metal",
			mt:       machineTypeWithBundledSSDs(32),
			wantConf: true,
		},
		{
			name:     "accelerator a3-highgpu-8g + top-level count → conflict",
			nc:       &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 1}},
			mtName:   "a3-highgpu-8g",
			mt:       machineTypeWithBundledSSDs(16),
			wantConf: true,
		},
		{
			name:     "accelerator a3-ultragpu-8g-nolssd + top-level count → no conflict (API says not bundled)",
			nc:       &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 2}},
			mtName:   "a3-ultragpu-8g-nolssd",
			mt:       machineTypeWithBundledSSDs(0),
			wantConf: false,
		},
		{
			name:     "accelerator + cache miss falls back to prefix → conflict",
			nc:       &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 1}},
			mtName:   "a4x-highgpu-4g",
			mt:       nil,
			wantConf: true,
		},
		{
			name:     "-nolssd sibling + cache miss falls back to suffix exclusion → no conflict",
			nc:       &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 1}},
			mtName:   "a3-ultragpu-8g-nolssd",
			mt:       nil,
			wantConf: false,
		},
		{
			name:     "bundled + no user SSD config → no conflict",
			nc:       &v1alpha1.GCENodeClass{},
			mtName:   "c4d-standard-8-lssd",
			mt:       machineTypeWithBundledSSDs(2),
			wantConf: false,
		},
		{
			name:     "non-bundled + top-level count → no conflict",
			nc:       &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 2}},
			mtName:   "n2d-standard-8",
			mt:       machineTypeWithBundledSSDs(0),
			wantConf: false,
		},
		{
			name:     "non-bundled + no user SSD config → no conflict",
			nc:       &v1alpha1.GCENodeClass{},
			mtName:   "n2d-standard-8",
			mt:       machineTypeWithBundledSSDs(0),
			wantConf: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := evaluateLocalSSDConflict(tc.nc, tc.mtName, tc.mt)
			if tc.wantConf {
				require.Error(t, err, "expected conflict error")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
