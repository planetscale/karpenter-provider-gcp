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

package localssd_test

import (
	"os"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var _ = DescribeTable("Local SSD",
	func(ctx SpecContext, tc environment.TestCase) {
		if slices.Contains(tc.Families, "z3") && os.Getenv("E2E_Z3_TESTS") != "true" {
			Skip("set E2E_Z3_TESTS=true to run z3 capacity-constrained tests")
		}
		env.RunProvisioningTest(ctx, tc)
	},
	// Bundled-SSD families: SSD count is fixed by the machine type;
	// LocalSsdMode still controls exposure.
	Entry("bundled c4-standard-8-lssd / RawBlock", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureAmd64,
		Families:             []string{"c4"},
		InstanceTypes:        []string{"c4-standard-8-lssd"},
		BootDiskCategory:     "hyperdisk-balanced",
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeRawBlock,
		ExpectedScratchDisks: 1,
	}, SpecTimeout(15*time.Minute)),
	Entry("bundled c4-standard-8-lssd / Ephemeral", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureAmd64,
		Families:             []string{"c4"},
		InstanceTypes:        []string{"c4-standard-8-lssd"},
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

	// Pod has no SSD-count label on a configurable family: zero SSDs
	// attached; the node is still stamped instance-local-ssd-count=0.
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
	// constraint; the cheaper no-SSD variant wins. Zero SSDs, node stamped 0.
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
	// asserted by the `bundled c4-standard-8-lssd` and `bundled
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
	// NodePool's In:["4"] requirement filters the n2d-standard-8 variants down to
	// the count=4 variant, which the provider then launches.
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

	// Ephemeral capacity-only: the pod requests ephemeral-storage and pins no
	// SSD-count label. The provider re-applies resource fit across the
	// n2d-standard-8 variants (count 0/1/2 advertise < 800Gi ephemeral-storage and
	// are filtered out) and the count-ascending tie-break selects the smallest
	// sufficient count, 4 (4 × 375 GiB = 1500 GiB). Exercises provider-side
	// resource fit + ordering, the path the old resolver couldn't satisfy.
	Entry("flex n2d / Ephemeral / capacity-only picks smallest sufficient count", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureAmd64,
		Families:             []string{"n2d"},
		InstanceTypes:        []string{"n2d-standard-8"},
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeEphemeral,
		PodEphemeralStorage:  "800Gi",
		ExpectedScratchDisks: 4,
	}, SpecTimeout(15*time.Minute)),

	// Multi-valued NodePool count requirement with no pod-side pin. The NodeClaim
	// inherits In:["0","1","2","4"]; under the pre-refactor resolver this surfaced
	// as MultiValuedLocalSSDCount and failed launch. The variant model treats it
	// as the allowed set and the count-ascending tie-break selects count 0.
	Entry("flex n2d / RawBlock / multi-valued NodePool count, no pod pin / count 0", environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2d"},
		InstanceTypes: []string{"n2d-standard-8"},
		LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "In",
			"values":   []any{"0", "1", "2", "4"},
		}},
	}, SpecTimeout(15*time.Minute)),

	// No-local-SSD pool: an explicit count In:["0"] requirement excludes the
	// bundled-SSD SKU (c4-standard-8-lssd emits count In:["1"]; the intersection is
	// empty) regardless of relative cost, leaving only the no-SSD c4-standard-2.
	// Zero SCRATCH disks proves the bundled SKU was not selected.
	Entry("family=c4 / NodePool count In:[0] excludes bundled lssd SKU", environment.TestCase{
		CapacityType:     karpv1.CapacityTypeOnDemand,
		Arch:             karpv1.ArchitectureAmd64,
		Families:         []string{"c4"},
		InstanceTypes:    []string{"c4-standard-2", "c4-standard-8-lssd"},
		BootDiskCategory: "hyperdisk-balanced",
		LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
		ExtraRequirements: []map[string]any{{
			"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
			"operator": "In",
			"values":   []any{"0"},
		}},
	}, SpecTimeout(15*time.Minute)),

	// Ubuntu parity. The local-SSD kube-env/kube-labels keys we emit are acted
	// on by GKE's per-OS bootstrapper, so the format/mount half of the path is
	// out of reach of unit tests. The entries above all run on COS; these two
	// prove the Ubuntu image honors the same keys for both modes: RawBlock
	// block-device exposure and Ephemeral format/stripe/mount.
	Entry("flex n2d / Ubuntu / RawBlock / pod-set 4 SSDs", environment.TestCase{
		CapacityType:     karpv1.CapacityTypeOnDemand,
		Arch:             karpv1.ArchitectureAmd64,
		Families:         []string{"n2d"},
		InstanceTypes:    []string{"n2d-standard-8"},
		ImageFamily:      gcpv1alpha1.ImageFamilyUbuntu,
		LocalSSDMode:     gcpv1alpha1.LocalSSDModeRawBlock,
		PodLocalSSDCount: "4",
	}, SpecTimeout(15*time.Minute)),
	Entry("flex n2d / Ubuntu / Ephemeral / capacity-only picks smallest sufficient count", environment.TestCase{
		CapacityType:         karpv1.CapacityTypeOnDemand,
		Arch:                 karpv1.ArchitectureAmd64,
		Families:             []string{"n2d"},
		InstanceTypes:        []string{"n2d-standard-8"},
		ImageFamily:          gcpv1alpha1.ImageFamilyUbuntu,
		LocalSSDMode:         gcpv1alpha1.LocalSSDModeEphemeral,
		PodEphemeralStorage:  "800Gi",
		ExpectedScratchDisks: 4,
	}, SpecTimeout(15*time.Minute)),
)
