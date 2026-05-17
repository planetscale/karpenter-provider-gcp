/*
Copyright 2024 The CloudPilot AI Authors.

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

package provisioning_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = DescribeTable("Provisioning",
	func(ctx SpecContext, tc environment.TestCase) {
		runProvisioningTest(ctx, tc)
	},
	// ContainerOptimizedOS
	Entry("COS / amd64 / on-demand", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
	}, SpecTimeout(15*time.Minute)),
	Entry("COS / arm64 / on-demand", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureArm64,
		Families:      []string{"c4a", "t2a"},
		InstanceTypes: []string{"c4a-standard-2", "c4a-standard-4", "t2a-standard-2"},
	}, SpecTimeout(15*time.Minute)),
	Entry("COS / amd64 / spot", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeSpot,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
	}, SpecTimeout(15*time.Minute)),
	Entry("COS / arm64 / spot", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeSpot,
		Arch:          karpv1.ArchitectureArm64,
		Families:      []string{"c4a", "t2a"},
		InstanceTypes: []string{"c4a-standard-2", "c4a-standard-4", "t2a-standard-2"},
	}, SpecTimeout(15*time.Minute)),
	// Ubuntu
	Entry("Ubuntu / amd64 / on-demand", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
	}, SpecTimeout(15*time.Minute)),
	Entry("Ubuntu / arm64 / on-demand", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureArm64,
		Families:      []string{"c4a", "t2a"},
		InstanceTypes: []string{"c4a-standard-2", "c4a-standard-4", "t2a-standard-2"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
	}, SpecTimeout(15*time.Minute)),
	Entry("Ubuntu / amd64 / spot", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeSpot,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
	}, SpecTimeout(15*time.Minute)),
	Entry("Ubuntu / arm64 / spot", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeSpot,
		Arch:          karpv1.ArchitectureArm64,
		Families:      []string{"c4a", "t2a"},
		InstanceTypes: []string{"c4a-standard-2", "c4a-standard-4", "t2a-standard-2"},
		ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
	}, SpecTimeout(15*time.Minute)),
	// Legacy disk-entry path: `disks: [{category: local-ssd}, ...]` still
	// produces NVMe SCRATCH disks (regression guard for the SCRATCH-attach
	// fix; deprecated in v1alpha1, removed at v1beta1).
	Entry("COS / amd64 / on-demand + 2 local SSDs (legacy)", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2"},
		LocalSSDCount: 2,
	}, SpecTimeout(15*time.Minute)),

	// Top-level LocalSsdMode / LocalSsdCount path. Flex families (n2d):
	// user picks the count.
	Entry("flex n2d / RawBlock / 1 SSD", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		LocalSSDCount: 1,
	}, SpecTimeout(15*time.Minute)),
	Entry("flex n2d / Ephemeral / 2 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeEphemeral,
		LocalSSDCount: 2,
	}, SpecTimeout(15*time.Minute)),

	// Bundled-SSD families: count is fixed by the machine type. User must
	// leave LocalSsdCount unset; LocalSsdMode still controls exposure.
	Entry("bundled c4d-standard-8-lssd / RawBlock", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureAmd64,
		Families:             []string{"c4d"},
		InstanceTypes:        []string{"c4d-standard-8-lssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeRawBlock,
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),
	Entry("bundled c4d-standard-8-lssd / Ephemeral", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureAmd64,
		Families:             []string{"c4d"},
		InstanceTypes:        []string{"c4d-standard-8-lssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeEphemeral,
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),
	Entry("bundled z3-highmem-22-standardlssd / Ephemeral", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureAmd64,
		Families:             []string{"z3"},
		InstanceTypes:        []string{"z3-highmem-22-standardlssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeEphemeral,
		ExpectedScratchDisks: 2,
	}, SpecTimeout(15*time.Minute)),
)
