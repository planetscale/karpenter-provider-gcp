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
	// Bundled-SSD families: SSD count is fixed by the machine type. User must
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
	Entry("bundled z3-highmem-14-standardlssd / Ephemeral", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureAmd64,
		Families:             []string{"z3"},
		InstanceTypes:        []string{"z3-highmem-14-standardlssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeEphemeral,
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),
	// c4a-standard-4-lssd bundles 1 SSD. c4a requires hyperdisk-balanced.
	Entry("bundled c4a-standard-4-lssd / arm64 / Ephemeral", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureArm64,
		Families:             []string{"c4a"},
		InstanceTypes:        []string{"c4a-standard-4-lssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeEphemeral,
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),

	// Pod sets karpenter.k8s.gcp/instance-local-ssd-count in NodeSelector;
	// configurable family attaches that many SSDs and labels the node.
	Entry("flex n2d / RawBlock / pod-set 4 SSDs", environment.TestCase{
		CapacityType:     karpv1.CapacityTypeOnDemand,
		Arch:             karpv1.ArchitectureAmd64,
		Families:         []string{"n2d"},
		InstanceTypes:    []string{"n2d-standard-8"},
		LocalSSDMode:     gcpv1alpha1.LocalSSDModeRawBlock,
		PodLocalSSDCount: "4",
	}, SpecTimeout(15*time.Minute)),
	// Ephemeral mode crossed with the pod-driven path; same flow as the
	// RawBlock entry above with a different SSD exposure mode.
	Entry("flex n2d / Ephemeral / pod-set 2 SSDs", environment.TestCase{
		CapacityType:     karpv1.CapacityTypeOnDemand,
		Arch:             karpv1.ArchitectureAmd64,
		Families:         []string{"n2d"},
		InstanceTypes:    []string{"n2d-standard-8"},
		LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
		PodLocalSSDCount: "2",
	}, SpecTimeout(15*time.Minute)),
	// Ubuntu equivalent of the RawBlock pod-set entry above. The SSD-count
	// label is written by the image's bootstrap script (separate code path
	// from COS).
	Entry("Ubuntu / amd64 / n2d / pod-set 4 SSDs", environment.TestCase{
		CapacityType:     karpv1.CapacityTypeOnDemand,
		Arch:             karpv1.ArchitectureAmd64,
		Families:         []string{"n2d"},
		InstanceTypes:    []string{"n2d-standard-8"},
		ImageFamily:      gcpv1alpha1.ImageFamilyUbuntu,
		LocalSSDMode:     gcpv1alpha1.LocalSSDModeRawBlock,
		PodLocalSSDCount: "4",
	}, SpecTimeout(15*time.Minute)),

	// Pod has no SSD-count label on a configurable family: zero SSDs
	// attached, no SSD-count label on the node.
	Entry("flex n2d / no SSD-count label / zero SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-2"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
	}, SpecTimeout(15*time.Minute)),

	// Family pin spanning a no-SSD variant (c4-standard-2) and a bundled-SSD
	// variant (c4-standard-8-lssd). Scheduler picks by cost.
	//
	// Pod has no SSD-count label: both variants pass the pod's empty
	// constraint; the cheaper no-SSD variant wins. Zero SSDs, no label.
	Entry("family=c4 + no SSD-count label / picks no-SSD variant", environment.TestCase{
		CapacityType:     karpv1.CapacityTypeOnDemand,
		Arch:             karpv1.ArchitectureAmd64,
		Families:         []string{"c4"},
		InstanceTypes:    []string{"c4-standard-2", "c4-standard-8-lssd"},
		BootDiskCategory: "hyperdisk-balanced",
		LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
	}, SpecTimeout(15*time.Minute)),
	// Pod requests SSD-count=1: the no-SSD variants emit
	// instance-local-ssd-count In:["0"] and are filtered out; only the
	// bundled-SSD variant remains. 1 SSD, label "1".
	//
	// Label-source ambiguity: pod's requested count (1) equals the bundled
	// count (c4-*-lssd bundles 1), so the label assertion alone can't tell
	// whether the label came from the pod's request or from the resolver
	// reading bundled count. Bundled-count label propagation is independently
	// asserted by the `bundled c4d-standard-8-lssd` and `bundled
	// c4a-standard-4-lssd` entries above, where no pod-side count is set.
	Entry("family=c4a + pod SSD-count=1 / picks lssd variant", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureArm64,
		Families:             []string{"c4a"},
		InstanceTypes:        []string{"c4a-standard-2", "c4a-standard-4-lssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeEphemeral,
		PodLocalSSDCount:     "1",
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),

	// SSD-count requirement lives on the NodePool template, not the pod. The
	// resolver reads the SSD count from the NodeClaim's inherited requirement.
	Entry("NodePool SSD-count=4 / pod has no SSD-count label / n2d gets 4 SSDs", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "In",
			"values":   []any{"4"},
		}},
		ExpectedScratchDisks: 4,
	}, SpecTimeout(15*time.Minute)),
)
