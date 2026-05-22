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
	"context"
	"regexp"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

var cosVersionFromImageRe = regexp.MustCompile(`cos-(\d+-\d+-\d+-\d+)-c-pre`)

func osSlug(imageFamily string) string {
	if imageFamily == gcpv1alpha1.ImageFamilyUbuntu {
		return "ubuntu"
	}
	return "cos"
}

func runProvisioningTest(ctx context.Context, tc environment.TestCase) {
	prefix := environment.TestPrefix(tc.Arch, tc.CapacityType, osSlug(tc.ImageFamily), "provisioning")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] arch=%s capacityType=%s os=%s nodePool=%s\n",
		tc.Arch, tc.CapacityType, tc.ImageFamily, name)

	initialNodes := env.AllNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	imageFamily := tc.ImageFamily
	if imageFamily == "" {
		imageFamily = gcpv1alpha1.ImageFamilyContainerOptimizedOS
	}
	if tc.LocalSSDMode != "" {
		env.CreateNodeClassForLocalSSD(ctx, name, imageFamily, tc.BootDiskCategory, tc.LocalSSDMode)
	} else {
		env.CreateNodeClass(ctx, name, imageFamily)
	}
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, tc)
	env.WaitForNodePoolReady(ctx, name)
	deployOpts := environment.DeploymentOptions{}
	if tc.PodLocalSSDCount != "" {
		deployOpts.ExtraNodeSelectors = map[string]string{
			gcpv1alpha1.LabelInstanceLocalSsdCount: tc.PodLocalSSDCount,
		}
	}
	env.CreateDeploymentWithOptions(ctx, name, name, name, tc.Arch, deployOpts)

	env.WaitForNodeClaimLaunched(ctx, name)
	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
	Expect(node.Labels[karpv1.NodeRegisteredLabelKey]).To(Equal("true"))
	Expect(node.Labels[karpv1.NodePoolLabelKey]).To(Equal(name))
	Expect(node.Labels[karpv1.CapacityTypeLabelKey]).To(Equal(tc.CapacityType))
	Expect(node.Labels[corev1.LabelArchStable]).To(Equal(tc.Arch))
	Expect(tc.Families).To(ContainElement(node.Labels[gcpv1alpha1.LabelInstanceFamily]))
	Expect(tc.InstanceTypes).To(ContainElement(node.Labels[corev1.LabelInstanceTypeStable]))

	expectedScratch := expectedScratchDiskCount(tc)
	inst, err := env.GetGCEInstance(ctx, node.Spec.ProviderID)
	Expect(err).NotTo(HaveOccurred(), "fetching GCE instance for %s", node.Name)
	var scratch int
	for _, d := range inst.Disks {
		if d.Type == "SCRATCH" && d.Interface == "NVME" {
			scratch++
		}
	}
	Expect(scratch).To(Equal(expectedScratch),
		"expected %d local SSDs on %s, got %d", expectedScratch, node.Name, scratch)

	if expectedScratch > 0 {
		// GKE-applied SSD labels (`cloud.google.com/gke-local-nvme-ssd`,
		// `cloud.google.com/gke-ephemeral-storage-local-ssd`) are written by
		// the bootstrapper and can land after the workload pod reaches Running,
		// so poll instead of asserting against the node snapshot fetched above.
		labelKey := "cloud.google.com/gke-local-nvme-ssd"
		if tc.LocalSSDMode == gcpv1alpha1.LocalSSDModeEphemeral {
			labelKey = "cloud.google.com/gke-ephemeral-storage-local-ssd"
		}
		Eventually(func(g Gomega) {
			n, err := env.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(n.Labels[labelKey]).To(Equal("true"),
				"expected %s=true node label", labelKey)
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
	}

	expectedCountLabel := expectedSSDCountLabelValue(tc)
	Eventually(func(g Gomega) {
		n, err := env.KubeClient.CoreV1().Nodes().Get(ctx, provisionedNodeName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		got := n.Labels[gcpv1alpha1.LabelInstanceLocalSsdCount]
		g.Expect(got).To(Equal(expectedCountLabel),
			"node %s: %s = %q, want %q",
			provisionedNodeName, gcpv1alpha1.LabelInstanceLocalSsdCount, got, expectedCountLabel)
	}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

	env.WaitForKubeProxyRunning(ctx, provisionedNodeName)
}

// expectedSSDCountLabelValue derives the expected value of the
// karpenter.k8s.gcp/instance-local-ssd-count label on the provisioned node.
// Variant emission writes the label for every count, including 0, so a
// no-SSD node carries "0" rather than an absent label.
func expectedSSDCountLabelValue(tc environment.TestCase) string {
	switch {
	case tc.PodLocalSSDCount != "":
		return tc.PodLocalSSDCount
	case tc.ExpectedScratchDisks > 0:
		return strconv.Itoa(tc.ExpectedScratchDisks)
	default:
		return "0"
	}
}

// expectedScratchDiskCount derives the expected number of SCRATCH NVMe disks
// attached to the provisioned GCE instance. Returns 0 when no SSDs are
// expected (also a valid assertion target — we always verify the disk count).
func expectedScratchDiskCount(tc environment.TestCase) int {
	switch {
	case tc.ExpectedScratchDisks > 0:
		return tc.ExpectedScratchDisks
	case tc.PodLocalSSDCount != "":
		n, err := strconv.Atoi(tc.PodLocalSSDCount)
		if err != nil || n < 0 {
			return 0
		}
		return n
	default:
		return 0
	}
}

// runPinnedAliasTest provisions a node using a GCENodeClass with the given
// alias and verifies the resolved source image contains expectedImageSubstring.
func runPinnedAliasTest(ctx context.Context, alias, expectedImageSubstring string) {
	prefix := "amd64-od-pinned"
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] alias=%s nodePool=%s\n", alias, name)

	initialNodes := env.AllNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	env.CreateNodeClassWithAlias(ctx, name, alias)
	env.WaitForNodeClassReady(ctx, name)

	images := env.NodeClassSourceImages(ctx, name)
	Expect(images).NotTo(BeEmpty(), "GCENodeClass %s has no status.images after becoming Ready", name)
	Expect(images[0]).To(ContainSubstring(expectedImageSubstring),
		"expected source image to contain %q, got %q", expectedImageSubstring, images[0])

	env.CreateNodePool(ctx, name, name, environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
	})
	env.WaitForNodePoolReady(ctx, name)
	env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

	env.WaitForNodeClaimLaunched(ctx, name)
	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)

	env.WaitForKubeProxyRunning(ctx, provisionedNodeName)
}

// runImageIDTest provisions a node using a GCENodeClass that selects an image
// by exact resource URL (id selector) and verifies the resolved source image
// matches the provided imageID exactly.
func runImageIDTest(ctx context.Context, imageFamily, imageID string) {
	prefix := "amd64-od-imageid"
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] imageFamily=%s imageID=%s nodePool=%s\n", imageFamily, imageID, name)

	initialNodes := env.AllNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	env.CreateNodeClassWithImageID(ctx, name, imageFamily, imageID)
	env.WaitForNodeClassReady(ctx, name)

	images := env.NodeClassSourceImages(ctx, name)
	Expect(images).NotTo(BeEmpty(), "GCENodeClass %s has no status.images after becoming Ready", name)
	Expect(images[0]).To(Equal(imageID),
		"expected source image to equal %q, got %q", imageID, images[0])

	env.CreateNodePool(ctx, name, name, environment.TestCase{
		CapacityType:  karpv1.CapacityTypeOnDemand,
		Arch:          karpv1.ArchitectureAmd64,
		Families:      []string{"n2"},
		InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
	})
	env.WaitForNodePoolReady(ctx, name)
	env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

	env.WaitForNodeClaimLaunched(ctx, name)
	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)

	env.WaitForKubeProxyRunning(ctx, provisionedNodeName)
}
