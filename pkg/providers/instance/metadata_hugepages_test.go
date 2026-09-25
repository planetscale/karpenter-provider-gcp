/*
Copyright 2026 The CloudPilot AI Authors.

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
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

func TestBuildInstance_HugepagesMetadata(t *testing.T) {
	provider := makeProvider()
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		LinuxNodeConfig: &v1alpha1.LinuxNodeConfig{
			Hugepages: &v1alpha1.HugepagesConfig{HugepageSize2m: lo.ToPtr[int32](4301)},
		},
	}}
	// The node class sets no 1 GiB pages, so the source pool's 1 GiB pages
	// must not reach the node.
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey: "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey: `HUGEPAGE_1G: "16"
KUBELET_ARGS: --max-pods=110 --node-labels=max-pods-per-node=110,max-pods=110
`,
		metadata.KubeletConfigKey: `nodeStatusUpdateFrequency: 10s
`,
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), sourceMetadata,
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-hugepages-test",
		karpv1.CapacityTypeOnDemand,
	)

	require.NoError(t, err)
	kubeEnv := kubeEnvFrom(t, instance)
	require.Contains(t, kubeEnv, `HUGEPAGE_2M: "4301"`)
	require.Contains(t, kubeEnv, `ENABLE_CONTAINERD_HUGETLB_CONTROLLER: "true"`)
	require.NotContains(t, kubeEnv, "HUGEPAGE_1G")
}

func TestBuildInstance_DropsStaleSourceHugepagesMetadata(t *testing.T) {
	provider := makeProvider()
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey: "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey: `HUGEPAGE_2M: "256000"
HUGEPAGE_1G: "16"
KUBELET_ARGS: --max-pods=110 --node-labels=max-pods-per-node=110,max-pods=110
`,
		metadata.KubeletConfigKey: `nodeStatusUpdateFrequency: 10s
`,
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), &v1alpha1.GCENodeClass{}, makeNonGPUIT(), sourceMetadata,
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-hugepages-test",
		karpv1.CapacityTypeOnDemand,
	)

	require.NoError(t, err)
	kubeEnv := kubeEnvFrom(t, instance)
	require.NotContains(t, kubeEnv, "HUGEPAGE_2M")
	require.NotContains(t, kubeEnv, "HUGEPAGE_1G")
}
