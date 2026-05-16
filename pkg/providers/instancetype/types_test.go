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

package instancetype

import (
	"context"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
)

// TestListEphemeralStorageCacheIsolation verifies that two GCENodeClass objects
// with different boot disk sizes but the same KubeletConfiguration receive
// independently computed ephemeral-storage overhead values from List().
//
// Without the disksHash in the cache key both calls share one entry, so the
// 30 GiB class would silently inherit the 200 GiB reservation (76 Gi) and the
// kubelet would refuse to start because 76 Gi > actual disk capacity (~25 Gi).
func TestListEphemeralStorageCacheIsolation(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	p := newTestProvider()

	nodeClass200 := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			Disks: []v1alpha1.Disk{
				{Boot: true, SizeGiB: 200, Category: "hyperdisk-balanced"},
			},
		},
	}
	nodeClass30 := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			Disks: []v1alpha1.Disk{
				{Boot: true, SizeGiB: 30, Category: "hyperdisk-balanced"},
			},
		},
	}

	// First call: 200 GiB disk – populates the cache.
	its200, err := p.List(ctx, nodeClass200)
	assert.NoError(t, err)
	assert.NotEmpty(t, its200)
	ephemeral200 := its200[0].Overhead.KubeReserved.StorageEphemeral()

	// Second call: 30 GiB disk – must NOT reuse the 200 GiB cache entry.
	its30, err := p.List(ctx, nodeClass30)
	assert.NoError(t, err)
	assert.NotEmpty(t, its30)
	ephemeral30 := its30[0].Overhead.KubeReserved.StorageEphemeral()

	assert.Equal(t, int64(76)*1024*1024*1024, ephemeral200.Value(),
		"200 GiB disk should produce 76 Gi kubeReserved ephemeral-storage")
	assert.Equal(t, int64(15)*1024*1024*1024, ephemeral30.Value(),
		"30 GiB disk should produce 15 Gi kubeReserved ephemeral-storage, not the cached 76 Gi")
	assert.Equal(t, 2, p.instanceTypesCache.ItemCount(),
		"each distinct disk config must produce a separate cache entry")
}

// TestListCacheKeyCoversLocalSSDFields verifies that two GCENodeClass objects
// differing only in LocalSsdMode (or only in LocalSsdCount) get independent
// InstanceType cache entries.
//
// Without this differentiation, switching a NodeClass from RawBlock to
// Ephemeral (or changing LocalSsdCount) would silently inherit the previous
// run's capacity/overhead from the cache for up to 30h.
func TestListCacheKeyCoversLocalSSDFields(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})

	t.Run("differing LocalSsdMode produces independent cache entries", func(t *testing.T) {
		p := newTestProvider()
		raw := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
			LocalSsdMode: v1alpha1.LocalSSDModeRawBlock, LocalSsdCount: 1,
		}}
		eph := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
			LocalSsdMode: v1alpha1.LocalSSDModeEphemeral, LocalSsdCount: 1,
		}}

		_, err := p.List(ctx, raw)
		assert.NoError(t, err)
		_, err = p.List(ctx, eph)
		assert.NoError(t, err)

		assert.Equal(t, 2, p.instanceTypesCache.ItemCount(),
			"different LocalSsdMode must produce different cache entries")
	})

	t.Run("differing LocalSsdCount produces independent cache entries", func(t *testing.T) {
		p := newTestProvider()
		nc1 := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 1}}
		nc2 := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LocalSsdCount: 2}}

		_, err := p.List(ctx, nc1)
		assert.NoError(t, err)
		_, err = p.List(ctx, nc2)
		assert.NoError(t, err)

		assert.Equal(t, 2, p.instanceTypesCache.ItemCount(),
			"different LocalSsdCount must produce different cache entries")
	})
}

func TestCalculateDiskConfiguration(t *testing.T) {
	tests := []struct {
		name             string
		nodeClass        *v1alpha1.GCENodeClass
		mt               *computepb.MachineType
		expectedBootGiB  int64
		expectedSSDGiB   int64
		expectedSSDCount int64
	}{
		{
			name: "30GiB boot disk from nodeClass (issue #220)",
			nodeClass: &v1alpha1.GCENodeClass{
				Spec: v1alpha1.GCENodeClassSpec{
					Disks: []v1alpha1.Disk{
						{Boot: true, SizeGiB: 30, Category: "hyperdisk-balanced"},
					},
				},
			},
			mt:               &computepb.MachineType{},
			expectedBootGiB:  30,
			expectedSSDGiB:   0,
			expectedSSDCount: 0,
		},
		{
			name:             "default 100GiB when no disks specified",
			nodeClass:        &v1alpha1.GCENodeClass{},
			mt:               &computepb.MachineType{},
			expectedBootGiB:  100,
			expectedSSDGiB:   0,
			expectedSSDCount: 0,
		},
		{
			name:      "BundledLocalSsds standard family: 2 partitions × 375 GiB",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: aws.String("n2-standard-8"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: aws.Int32(2),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   750,
			expectedSSDCount: 2,
		},
		{
			name:      "BundledLocalSsds z3 family: 4 partitions × 3000 GiB",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: aws.String("z3-highmem-88-standardlssd"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: aws.Int32(4),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   12000,
			expectedSSDCount: 4,
		},
		{
			name:      "BundledLocalSsds PartitionCount=0 treated as no SSDs",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: aws.String("n2-standard-8"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: aws.Int32(0),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   0,
			expectedSSDCount: 0,
		},
		{
			// Empirically verified: c4d-highmem-8-lssd ships 1 × 375 GiB SSD,
			// matching the API's PartitionCount. The previous 2250 GiB override
			// was empirically wrong; removing it lets the default per-partition
			// math apply.
			name:      "c4d-highmem-8-lssd: 1 partition x 375 GiB",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: aws.String("c4d-highmem-8-lssd"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: aws.Int32(1),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   375,
			expectedSSDCount: 1,
		},
		{
			name:      "c4d-highmem-16-lssd: 1 partition x 375 GiB",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: aws.String("c4d-highmem-16-lssd"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: aws.Int32(1),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   375,
			expectedSSDCount: 1,
		},
		{
			// Top-level spec.localSsdCount on a non-bundled family must drive
			// the SSD count, with size defaulting to the per-family partition
			// size (375 GiB on n2d). Boot disk default applies because no
			// boot entry is set.
			name: "top-level LocalSsdCount=2 on n2d (no Disks)",
			nodeClass: &v1alpha1.GCENodeClass{
				Spec: v1alpha1.GCENodeClassSpec{
					LocalSsdCount: 2,
				},
			},
			mt: &computepb.MachineType{
				Name: aws.String("n2d-standard-4"),
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   750,
			expectedSSDCount: 2,
		},
		{
			// Regression for early-return removal: an explicit boot disk
			// entry alongside a bundled-SSD machine must surface BOTH the
			// boot size AND the bundled SSDs.
			name: "c4d-standard-8-lssd with custom 100 GiB boot disk",
			nodeClass: &v1alpha1.GCENodeClass{
				Spec: v1alpha1.GCENodeClassSpec{
					Disks: []v1alpha1.Disk{
						{Boot: true, SizeGiB: 100, Category: "hyperdisk-balanced"},
					},
				},
			},
			mt: &computepb.MachineType{
				Name: aws.String("c4d-standard-8-lssd"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: aws.Int32(1),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   375,
			expectedSSDCount: 1,
		},
		{
			// Top-level LocalSsdCount wins over legacy disk entries to avoid
			// double-counting when both are set.
			name: "top-level LocalSsdCount=3 wins over 1 legacy disk entry",
			nodeClass: &v1alpha1.GCENodeClass{
				Spec: v1alpha1.GCENodeClassSpec{
					LocalSsdCount: 3,
					Disks: []v1alpha1.Disk{
						{Boot: true, SizeGiB: 50, Category: "pd-balanced"},
						{Category: "local-ssd"},
					},
				},
			},
			mt: &computepb.MachineType{
				Name: aws.String("n2d-standard-4"),
			},
			expectedBootGiB:  50,
			expectedSSDGiB:   3 * 375,
			expectedSSDCount: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bootGiB, ssdGiB, ssdCount := calculateDiskConfigGiB(tt.nodeClass, tt.mt)
			assert.Equal(t, tt.expectedBootGiB, bootGiB, "boot disk GiB mismatch")
			assert.Equal(t, tt.expectedSSDGiB, ssdGiB, "total SSD GiB mismatch")
			assert.Equal(t, tt.expectedSSDCount, ssdCount, "SSD count mismatch")
		})
	}
}

// TestNewInstanceTypeModeAware verifies that Capacity[ephemeral-storage] and
// Overhead.KubeReserved[ephemeral-storage] are attributed correctly based on
// (machine type, LocalSsdMode, LocalSsdCount).
//
// Why this matters: RawBlock SSDs are NOT mounted as ephemeral storage, so
// they must not contribute to Capacity or to the SSD-mode (50/75/100 GiB)
// kubeReserved reservation. Today the calculation conflates the two and
// over-promises ephemeral capacity on default RawBlock -lssd nodes.
func TestNewInstanceTypeModeAware(t *testing.T) {
	const GiB = int64(1024) * 1024 * 1024

	// boot-mode kubeReserved at 100 GiB default boot disk:
	// min(50, round(35+6), 100) = 41 GiB.
	const bootModeReservedGiB = int64(41)
	const bootModeCapGiB = int64(100)

	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	offerings := cloudprovider.Offerings{{
		Available: true,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		),
	}}

	cases := []struct {
		name             string
		mt               *computepb.MachineType
		mode             v1alpha1.LocalSSDMode
		count            int32
		wantCapGiB       int64
		wantReservedGiB  int64
	}{
		{
			name:            "n2d-standard-4 no SSDs default (RawBlock)",
			mt:              &computepb.MachineType{Name: aws.String("n2d-standard-4"), GuestCpus: aws.Int32(4), MemoryMb: aws.Int32(16384)},
			mode:            "",
			count:           0,
			wantCapGiB:      bootModeCapGiB,
			wantReservedGiB: bootModeReservedGiB,
		},
		{
			name:            "n2d-standard-4 LocalSsdCount=2 RawBlock",
			mt:              &computepb.MachineType{Name: aws.String("n2d-standard-4"), GuestCpus: aws.Int32(4), MemoryMb: aws.Int32(16384)},
			mode:            v1alpha1.LocalSSDModeRawBlock,
			count:           2,
			wantCapGiB:      bootModeCapGiB,
			wantReservedGiB: bootModeReservedGiB,
		},
		{
			name:            "n2d-standard-4 LocalSsdCount=2 Ephemeral",
			mt:              &computepb.MachineType{Name: aws.String("n2d-standard-4"), GuestCpus: aws.Int32(4), MemoryMb: aws.Int32(16384)},
			mode:            v1alpha1.LocalSSDModeEphemeral,
			count:           2,
			wantCapGiB:      750,
			wantReservedGiB: 75,
		},
		{
			name: "c4d-standard-8-lssd default (RawBlock) — bundled SSD ignored for capacity",
			mt: &computepb.MachineType{
				Name: aws.String("c4d-standard-8-lssd"), GuestCpus: aws.Int32(8), MemoryMb: aws.Int32(32768),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: aws.Int32(1)},
			},
			mode:            "",
			count:           0,
			wantCapGiB:      bootModeCapGiB,
			wantReservedGiB: bootModeReservedGiB,
		},
		{
			name: "c4d-standard-8-lssd Ephemeral",
			mt: &computepb.MachineType{
				Name: aws.String("c4d-standard-8-lssd"), GuestCpus: aws.Int32(8), MemoryMb: aws.Int32(32768),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: aws.Int32(1)},
			},
			mode:            v1alpha1.LocalSSDModeEphemeral,
			count:           0,
			wantCapGiB:      375,
			wantReservedGiB: 50,
		},
		{
			name: "c4d-standard-96-lssd Ephemeral (8 partitions)",
			mt: &computepb.MachineType{
				Name: aws.String("c4d-standard-96-lssd"), GuestCpus: aws.Int32(96), MemoryMb: aws.Int32(393216),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: aws.Int32(8)},
			},
			mode:            v1alpha1.LocalSSDModeEphemeral,
			count:           0,
			wantCapGiB:      3000,
			wantReservedGiB: 100,
		},
		{
			name: "z3-highmem-88 legacy SKU Ephemeral (12 partitions × 3000 GiB)",
			mt: &computepb.MachineType{
				Name: aws.String("z3-highmem-88"), GuestCpus: aws.Int32(88), MemoryMb: aws.Int32(720896),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: aws.Int32(12)},
			},
			mode:            v1alpha1.LocalSSDModeEphemeral,
			count:           0,
			wantCapGiB:      36000,
			wantReservedGiB: 100,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := &v1alpha1.GCENodeClass{
				Spec: v1alpha1.GCENodeClassSpec{
					LocalSsdMode:  tc.mode,
					LocalSsdCount: tc.count,
				},
			}
			it := NewInstanceType(ctx, tc.mt, nc, "us-central1", offerings)
			assert.NotNil(t, it, "InstanceType should be non-nil")
			cap := it.Capacity[corev1.ResourceEphemeralStorage]
			res := it.Overhead.KubeReserved[corev1.ResourceEphemeralStorage]
			assert.Equal(t, tc.wantCapGiB*GiB, cap.Value(), "ephemeral-storage capacity")
			assert.Equal(t, tc.wantReservedGiB*GiB, res.Value(), "ephemeral-storage kubeReserved")
		})
	}
}

func TestComputeRequirements(t *testing.T) {
	tests := []struct {
		name      string
		mt        *computepb.MachineType
		offerings cloudprovider.Offerings
		region    string
		expected  scheduling.Requirements
	}{
		{
			name: "Standard Instance (n1-standard-1)",
			mt: &computepb.MachineType{
				Name:        aws.String("n1-standard-1"),
				GuestCpus:   aws.Int32(1),
				MemoryMb:    aws.Int32(3840),
				Zone:        aws.String("us-central1-a"),
				Description: aws.String("1 vCPU, 3.75 GB RAM"),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "n1-standard-1"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPUModel, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "3840"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCategory, corev1.NodeSelectorOpIn, "n"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "n1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "standard"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			),
		},
		{
			name: "ARM Instance (t2a-standard-1)",
			mt: &computepb.MachineType{
				Name:         aws.String("t2a-standard-1"),
				GuestCpus:    aws.Int32(1),
				MemoryMb:     aws.Int32(4096),
				Architecture: aws.String("ARM64"),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "t2a-standard-1"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPUModel, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "4096"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCategory, corev1.NodeSelectorOpIn, "t"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "t2a"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "standard"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"),
			),
		},
		{
			name: "GPU Instance (a2-highgpu-1g)",
			mt: &computepb.MachineType{
				Name:      aws.String("a2-highgpu-1g"),
				GuestCpus: aws.Int32(12),
				MemoryMb:  aws.Int32(86016),
				Accelerators: []*computepb.Accelerators{
					{
						GuestAcceleratorCount: aws.Int32(1),
						GuestAcceleratorType:  aws.String("nvidia-tesla-a100"),
					},
				},
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "a2-highgpu-1g"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "12"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPUModel, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "86016"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCategory, corev1.NodeSelectorOpIn, "a"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "a2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "highgpu"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "1g"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpIn, "nvidia-tesla-a100"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			),
		},
		{
			name: "E2 Instance (e2-medium)",
			mt: &computepb.MachineType{
				Name:      aws.String("e2-medium"),
				GuestCpus: aws.Int32(2),
				MemoryMb:  aws.Int32(4096),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "e2-medium"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPUModel, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "4096"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCategory, corev1.NodeSelectorOpIn, "e"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "e2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "medium"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "medium"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			),
		},
		{
			name: "Offering with ZoneID",
			mt: &computepb.MachineType{
				Name:      aws.String("n1-standard-1"),
				GuestCpus: aws.Int32(1),
				MemoryMb:  aws.Int32(3840),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
						scheduling.NewRequirement(v1alpha1.LabelTopologyZoneID, corev1.NodeSelectorOpIn, "us-central1-a-id"),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "n1-standard-1"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPUModel, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "3840"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCategory, corev1.NodeSelectorOpIn, "n"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "n1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "standard"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			),
		},
		{
			name: "GPU Instance (c3d-highmem-8-lssd)",
			mt: &computepb.MachineType{
				Name:      aws.String("c3d-highmem-8-lssd"),
				GuestCpus: aws.Int32(8),
				MemoryMb:  aws.Int32(65536),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "c3d-highmem-8-lssd"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "8"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPUModel, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "65536"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCategory, corev1.NodeSelectorOpIn, "c"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "c3d"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "highmem"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "3"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "8"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeRequirements(tt.mt, tt.offerings, tt.region)

			// Validate keys present in got
			for key := range got {
				if tt.expected.Get(key) == nil {
					// Ignore LabelTopologyZoneID if it wasn't expected (due to auto-generation with random values)
					if key == v1alpha1.LabelTopologyZoneID {
						continue
					}
					t.Errorf("Unexpected key in result: %s", key)
				}
			}

			// Validate keys present in expected
			for key, req := range tt.expected {
				gotReq := got.Get(key)
				assert.NotNil(t, gotReq, "requirement %s should exist", key)
				if gotReq != nil {
					assert.Equal(t, req.Operator(), gotReq.Operator(), "operator for %s should match", key)
					if req.Operator() == corev1.NodeSelectorOpIn {
						assert.ElementsMatch(t, req.Values(), gotReq.Values(), "values for %s should match", key)
					}
				}
			}
		})
	}
}
