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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestLocalSSDBundled(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// -lssd suffix family (c4d/c4a)
		{"c4d-standard-8-lssd", true},
		{"c4a-standard-72-lssd", true},
		{"c4d-standard-96-lssd", true},

		// -standardlssd suffix (z3)
		{"z3-highmem-22-standardlssd", true},
		{"z3-highmem-88-standardlssd", true},

		// -highlssd-metal suffix
		{"z3-highmem-192-highlssd-metal", true},

		// legacy z3 SKUs without explicit suffix still bundle SSDs
		{"z3-highmem-88", true},
		{"z3-highmem-22", true},

		// non-bundled families
		{"n2-standard-8", false},
		{"n2d-standard-8", false},
		{"c2-standard-4", false},
		{"m1-ultramem-40", false},
		{"e2-medium", false},

		// near-miss: "ssd" anywhere in the name shouldn't trigger
		{"some-ssd-family", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, localSSDBundled(tc.name))
		})
	}
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
		wantConf bool
	}{
		{
			name:     "bundled + top-level count → conflict",
			nc:       &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 1}},
			mtName:   "c4d-standard-8-lssd",
			wantConf: true,
		},
		{
			name: "bundled + legacy disk-entry → conflict",
			nc: &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
				Disks: []v1alpha1.Disk{{Category: "local-ssd"}},
			}},
			mtName:   "z3-highmem-22-standardlssd",
			wantConf: true,
		},
		{
			name:     "bundled legacy z3 + top-level count → conflict",
			nc:       &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 1}},
			mtName:   "z3-highmem-88",
			wantConf: true,
		},
		{
			name:     "bundled + no user SSD config → no conflict",
			nc:       &v1alpha1.GCENodeClass{},
			mtName:   "c4d-standard-8-lssd",
			wantConf: false,
		},
		{
			name:     "non-bundled + top-level count → no conflict",
			nc:       &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 2}},
			mtName:   "n2d-standard-8",
			wantConf: false,
		},
		{
			name:     "non-bundled + no user SSD config → no conflict",
			nc:       &v1alpha1.GCENodeClass{},
			mtName:   "n2d-standard-8",
			wantConf: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := evaluateLocalSSDConflict(tc.nc, tc.mtName)
			if tc.wantConf {
				require.Error(t, err, "expected conflict error")
				assert.True(t, errors.Is(err, err), "sanity: error wraps itself")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
