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
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// These cases assert failure-mode behavior (pod stays Pending, NodeClaim
// launch fails) and therefore cannot ride env.RunProvisioningTest, which asserts
// the pod reaches Running.

var _ = Describe("Local-SSD failure modes", func() {
	It("leaves the pod Pending when the pinned SSD-count mismatches a bundled SKU", func(ctx SpecContext) {
		name := newLocalSSDPool(ctx, environment.TestCase{
			CapacityType:     karpv1.CapacityTypeOnDemand,
			Arch:             karpv1.ArchitectureAmd64,
			Families:         []string{"z3"},
			InstanceTypes:    []string{"z3-highmem-22-standardlssd"},
			BootDiskCategory: "hyperdisk-balanced",
			LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
		})
		env.CreateDeploymentWithOptions(ctx, name, name, name, karpv1.ArchitectureAmd64,
			environment.DeploymentOptions{ExtraNodeSelectors: map[string]string{
				corev1.LabelInstanceTypeStable:         "z3-highmem-22-standardlssd",
				gcpv1alpha1.LabelInstanceLocalSsdCount: "4", // z3-...-22 bundles 2; 4 has no match
			}})
		DeferCleanup(func(ctx context.Context) { env.DeleteDeployment(ctx, name) })
		env.ConsistentlyExpectPendingPods(ctx, name, 30*time.Second)
		env.ExpectNoNodeClaim(ctx, name)
	}, SpecTimeout(10*time.Minute))

	It("leaves the pod Pending when the pod-requested SSD-count exceeds the family maximum", func(ctx SpecContext) {
		// n2d's published max per-vCPU table tops out at 24. 32 stays
		// above any plausible future bump and has no intersection with
		// any variant emitted for n2d-standard-2.
		name := newLocalSSDPool(ctx, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2d"},
			InstanceTypes: []string{"n2d-standard-2"},
			LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
		})
		env.CreateDeploymentWithOptions(ctx, name, name, name, karpv1.ArchitectureAmd64,
			environment.DeploymentOptions{ExtraNodeSelectors: map[string]string{
				corev1.LabelInstanceTypeStable:         "n2d-standard-2",
				gcpv1alpha1.LabelInstanceLocalSsdCount: "32",
			}})
		DeferCleanup(func(ctx context.Context) { env.DeleteDeployment(ctx, name) })
		env.ConsistentlyExpectPendingPods(ctx, name, 30*time.Second)
		env.ExpectNoNodeClaim(ctx, name)
	}, SpecTimeout(10*time.Minute))

	It("leaves the pod Pending on a no-SSD-only family pinned with an SSD-count label", func(ctx SpecContext) {
		// e2 doesn't support local SSDs at all. The scheduler emits
		// instance-local-ssd-count In:["0"] on its InstanceTypes; the pod's
		// In:["4"] doesn't intersect, so no InstanceType is compatible.
		name := newLocalSSDPool(ctx, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"e2"},
			InstanceTypes: []string{"e2-standard-2"},
		})
		env.CreateDeploymentWithOptions(ctx, name, name, name, karpv1.ArchitectureAmd64,
			environment.DeploymentOptions{ExtraNodeSelectors: map[string]string{
				corev1.LabelInstanceTypeStable:         "e2-standard-2",
				gcpv1alpha1.LabelInstanceLocalSsdCount: "4",
			}})
		DeferCleanup(func(ctx context.Context) { env.DeleteDeployment(ctx, name) })
		env.ConsistentlyExpectPendingPods(ctx, name, 30*time.Second)
		env.ExpectNoNodeClaim(ctx, name)
	}, SpecTimeout(10*time.Minute))

	It("leaves the pod Pending when pod SSD-count contradicts NodePool SSD-count", func(ctx SpecContext) {
		// NodePool pins count=4; pod pins count=2. Karpenter intersects the
		// requirements (In:["4"] ∩ In:["2"] = empty), no InstanceType is
		// compatible. Asserts the NodePool requirement is a hard constraint
		// rather than a default pods can override.
		name := newLocalSSDPool(ctx, environment.TestCase{
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
		})
		env.CreateDeploymentWithOptions(ctx, name, name, name, karpv1.ArchitectureAmd64,
			environment.DeploymentOptions{ExtraNodeSelectors: map[string]string{
				corev1.LabelInstanceTypeStable:         "n2d-standard-8",
				gcpv1alpha1.LabelInstanceLocalSsdCount: "2",
			}})
		DeferCleanup(func(ctx context.Context) { env.DeleteDeployment(ctx, name) })
		env.ConsistentlyExpectPendingPods(ctx, name, 30*time.Second)
		env.ExpectNoNodeClaim(ctx, name)
	}, SpecTimeout(10*time.Minute))

	It("leaves the pod Pending when NodePool SSD-count contradicts a bundled SKU's count", func(ctx SpecContext) {
		// NodePool pins count=4; z3-highmem-22-standardlssd's InstanceType
		// emits count In:["2"] (bundled). Intersection empty. Asserts that
		// NodePool-template requirements don't override InstanceType-emitted
		// bundled counts.
		name := newLocalSSDPool(ctx, environment.TestCase{
			CapacityType:     karpv1.CapacityTypeOnDemand,
			Arch:             karpv1.ArchitectureAmd64,
			Families:         []string{"z3"},
			InstanceTypes:    []string{"z3-highmem-22-standardlssd"},
			BootDiskCategory: "hyperdisk-balanced",
			LocalSSDMode:     gcpv1alpha1.LocalSSDModeEphemeral,
			ExtraRequirements: []map[string]any{{
				"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
				"operator": "In",
				"values":   []any{"4"},
			}},
		})
		env.CreateDeploymentWithOptions(ctx, name, name, name, karpv1.ArchitectureAmd64,
			environment.DeploymentOptions{})
		DeferCleanup(func(ctx context.Context) { env.DeleteDeployment(ctx, name) })
		env.ConsistentlyExpectPendingPods(ctx, name, 30*time.Second)
		env.ExpectNoNodeClaim(ctx, name)
	}, SpecTimeout(10*time.Minute))

})

// newLocalSSDPool creates the NodeClass + NodePool described by tc and
// registers cleanup. Returns the shared resource name. Used only by failure-
// mode tests in this file; positive cases run through env.RunProvisioningTest.
func newLocalSSDPool(ctx context.Context, tc environment.TestCase) string {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, "cos", "lssd")
	name := prefix + "-" + environment.UniqueSuffix()
	DeferCleanup(func(ctx context.Context) {
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
	})
	env.CreateNodeClassForLocalSSD(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		tc.BootDiskCategory, tc.LocalSSDMode)
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.WaitForNodePoolReady(ctx, name)
	return name
}
