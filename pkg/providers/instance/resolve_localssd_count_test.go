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
	"context"
	"net/http"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// stubInstanceTypeProvider implements instancetype.Provider with a static
// machine-type map. Only GetMachineType is exercised; the other methods are
// satisfied so the type implements the interface.
type stubInstanceTypeProvider struct {
	mts map[string]*computepb.MachineType
}

func (s *stubInstanceTypeProvider) GetMachineType(name string) *computepb.MachineType {
	return s.mts[name]
}

func (s *stubInstanceTypeProvider) LivenessProbe(*http.Request) error { return nil }
func (s *stubInstanceTypeProvider) List(context.Context, *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error) {
	return nil, nil
}
func (s *stubInstanceTypeProvider) UpdateInstanceTypes(context.Context) error         { return nil }
func (s *stubInstanceTypeProvider) UpdateInstanceTypeOfferings(context.Context) error { return nil }

// TestResolveLocalSSDCount pins the precedence used to advertise the local-SSD
// count to the GKE bootstrapper via kube-env. This precedence is intentionally
// inverted relative to instancetype.calculateDiskConfigGiB (which runs before
// the conflict check and therefore prefers user config). Locking the order
// here protects the kube-env writer against a future refactor that weakens
// evaluateLocalSSDConflict and lets a bundled+user-count combination through.
//
// Order: BundledLocalSsds (cache hit) > spec.LocalSsdCount > legacy disk
// entries. Cache miss for a name that the suffix predicate says bundles SSDs
// must return an error rather than a silent zero (#3 invariant).
func TestResolveLocalSSDCount(t *testing.T) {
	bundled := func(count int32) *computepb.MachineType {
		return &computepb.MachineType{
			BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr(count)},
		}
	}
	nonBundled := &computepb.MachineType{}

	cases := []struct {
		name             string
		mts              map[string]*computepb.MachineType
		nc               *v1alpha1.GCENodeClass
		instanceTypeName string
		want             int
		wantErr          bool
	}{
		{
			name:             "bundled cache hit wins over user count",
			mts:              map[string]*computepb.MachineType{"c4d-standard-8-lssd": bundled(2)},
			nc:               &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 7}},
			instanceTypeName: "c4d-standard-8-lssd",
			want:             2,
		},
		{
			name:             "bundled cache hit wins over legacy disks",
			mts:              map[string]*computepb.MachineType{"z3-highmem-22-standardlssd": bundled(12)},
			nc:               &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{Disks: []v1alpha1.Disk{{Category: "local-ssd"}, {Category: "local-ssd"}}}},
			instanceTypeName: "z3-highmem-22-standardlssd",
			want:             12,
		},
		{
			name:             "non-bundled cache hit + user count → user count",
			mts:              map[string]*computepb.MachineType{"n2d-standard-8": nonBundled},
			nc:               &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 4}},
			instanceTypeName: "n2d-standard-8",
			want:             4,
		},
		{
			name: "non-bundled cache hit + legacy disks → legacy count",
			mts:  map[string]*computepb.MachineType{"n2-standard-8": nonBundled},
			nc: &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{Disks: []v1alpha1.Disk{
				{Category: "local-ssd"},
				{Category: "local-ssd"},
				{Category: "local-ssd"},
			}}},
			instanceTypeName: "n2-standard-8",
			want:             3,
		},
		{
			name:             "non-bundled + no SSD config → 0",
			mts:              map[string]*computepb.MachineType{"n2d-standard-8": nonBundled},
			nc:               &v1alpha1.GCENodeClass{},
			instanceTypeName: "n2d-standard-8",
			want:             0,
		},
		{
			name:             "user count wins over legacy when both set",
			mts:              map[string]*computepb.MachineType{"n2d-standard-8": nonBundled},
			nc:               &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 2, Disks: []v1alpha1.Disk{{Category: "local-ssd"}, {Category: "local-ssd"}, {Category: "local-ssd"}}}},
			instanceTypeName: "n2d-standard-8",
			want:             2,
		},
		{
			name:             "cache miss + bundled suffix → error (#3 invariant)",
			mts:              map[string]*computepb.MachineType{},
			nc:               &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 1}},
			instanceTypeName: "c4d-standard-8-lssd",
			wantErr:          true,
		},
		{
			name:             "cache miss + non-bundled name → user count",
			mts:              map[string]*computepb.MachineType{},
			nc:               &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 4}},
			instanceTypeName: "n2d-standard-8",
			want:             4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &DefaultProvider{instanceTypeProvider: &stubInstanceTypeProvider{mts: tc.mts}}
			got, err := p.resolveLocalSSDCount(tc.nc, tc.instanceTypeName)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
