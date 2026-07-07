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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// Consolidation stability for same-name local-SSD variants. The proposal pins
// uniform base price across a configurable family's per-count variants so a
// running count>0 node is never seen as replaceable by a cheaper same-name
// count-0 sibling. This is the disruption-side half of that invariant.

var _ = Describe("Local-SSD consolidation", func() {
	It("does not consolidate a count-4 node onto a count-0 sibling", func(ctx SpecContext) {
		// NodePool allows both count 0 and count 4 for the same machine type, so a
		// count-0 sibling is a candidate the consolidator could pick if pricing
		// were not uniform. The pod pins count 4.
		pool := newLocalSSDPool(ctx, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2d"},
			InstanceTypes: []string{"n2d-standard-8"},
			LocalSSDMode:  gcpv1alpha1.LocalSSDModeRawBlock,
			ExtraRequirements: []map[string]any{{
				"key":      gcpv1alpha1.LabelInstanceLocalSsdCount,
				"operator": "In",
				"values":   []any{"0", "4"},
			}},
		})
		node := runPodOnPool(ctx, pool, "rawblock4", "n2d-standard-8", "4")
		expectNodeShape(ctx, node, "n2d-standard-8", 4, "4")

		// consolidateAfter=30s; hold past several evaluation windows. A uniform-priced
		// count-0 sibling must never look cheaper, so the count-4 node is never marked
		// for disruption.
		Consistently(func(g Gomega) {
			n, err := env.KubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "count-4 node %s disappeared", node.Name)
			g.Expect(n.DeletionTimestamp).To(BeNil(),
				"count-4 node %s was marked for disruption", node.Name)
		}, 90*time.Second, 5*time.Second).Should(Succeed())
	}, SpecTimeout(20*time.Minute))
})
