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

package cloudprovider

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

func TestInstanceToNodeClaim_PropagatesClusterLocationLabel(t *testing.T) {
	t.Parallel()

	inst := &instance.Instance{
		Name:      "test-node",
		ProjectID: "my-project",
		Location:  "us-central1-f",
		Labels:    map[string]string{utils.LabelClusterLocationKey: "us-central1-f"},
	}

	nc := (&CloudProvider{}).instanceToNodeClaim(inst, nil)

	require.Equal(t, "us-central1-f", nc.Labels[utils.LabelClusterLocationKey],
		"cluster-location label must be propagated from instance to NodeClaim so the GC controller can inspect it")
}

func TestInstanceToNodeClaim_AbsentClusterLocationLabelNotInvented(t *testing.T) {
	t.Parallel()

	// Legacy instances (created before this label was introduced) must not have
	// the label invented in their synthetic NodeClaim — the GC controller relies
	// on label absence to identify and skip these pre-migration nodes.
	inst := &instance.Instance{
		Name:         "legacy-node",
		ProjectID:    "my-project",
		Location:     "us-central1-f",
		CreationTime: time.Now().Add(-5 * time.Minute),
		Labels:       map[string]string{},
	}

	nc := (&CloudProvider{}).instanceToNodeClaim(inst, nil)

	_, hasLabel := nc.Labels[utils.LabelClusterLocationKey]
	require.False(t, hasLabel,
		"NodeClaim built from a label-less instance must not carry cluster-location; GC skip depends on its absence")
}

func TestDelete_ReturnsNodeClaimNotFoundWhenProviderIDEmpty(t *testing.T) {
	t.Parallel()

	// Without the guard in Delete, the call reaches parseGCEProviderID("") which errors,
	// causing karpenter to retry termination forever so the finalizer never clears.
	nc := &karpv1.NodeClaim{Status: karpv1.NodeClaimStatus{ProviderID: ""}}

	err := (&CloudProvider{}).Delete(context.Background(), nc)

	require.True(t, karpcloudprovider.IsNodeClaimNotFoundError(err),
		"empty providerID must signal NotFound so karpenter clears the finalizer; got %v", err)
}

// variantInstanceType is a minimal InstanceType for matchVariantForInstance
// tests: name + an `instance-local-ssd-count In:[count]` requirement.
func variantInstanceType(name string, count string) *karpcloudprovider.InstanceType {
	return &karpcloudprovider.InstanceType{
		Name: name,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, count),
		),
	}
}

// instanceWithSSDLabel constructs an Instance carrying the GCE label form of
// instance-local-ssd-count, mirroring what setupInstanceLabels writes at
// create time.
func instanceWithSSDLabel(instanceType, count string) *instance.Instance {
	return &instance.Instance{
		Type: instanceType,
		Labels: map[string]string{
			utils.SanitizeGCELabelValue(v1alpha1.LabelInstanceLocalSsdCount): count,
		},
	}
}

func TestMatchVariantForInstance_ByGCELabel(t *testing.T) {
	t.Parallel()
	// Configurable-SSD families emit one InstanceType per allowed count, all
	// sharing the same Name. The variant carrying the matching count must be
	// returned so cloudprovider.List/Get reconstruct NodeClaims with the
	// correct instance-local-ssd-count label.
	its := []*karpcloudprovider.InstanceType{
		variantInstanceType("n2d-standard-8", "0"),
		variantInstanceType("n2d-standard-8", "1"),
		variantInstanceType("n2d-standard-8", "2"),
		variantInstanceType("n2d-standard-8", "4"),
	}
	got, ok := matchVariantForInstance(its, instanceWithSSDLabel("n2d-standard-8", "2"))
	require.True(t, ok)
	req := got.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount)
	require.Equal(t, "2", req.Any(),
		"must pick the variant whose ssd-count requirement matches the instance's GCE label")
}

func TestMatchVariantForInstance_AbsentLabelFallsBackToFirstNameMatch(t *testing.T) {
	t.Parallel()
	// Instances created before this refactor (or adopted externally) carry no
	// SSD-count GCE label. Returning the first Name-match preserves the
	// prior behavior rather than erroring out and stalling reconciliation.
	its := []*karpcloudprovider.InstanceType{
		variantInstanceType("n2d-standard-8", "0"),
		variantInstanceType("n2d-standard-8", "2"),
	}
	inst := &instance.Instance{Type: "n2d-standard-8"} // no Labels
	got, ok := matchVariantForInstance(its, inst)
	require.True(t, ok)
	req := got.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount)
	require.Equal(t, "0", req.Any(),
		"missing SSD-count label must fall back to the first Name-match variant")
}

func TestMatchVariantForInstance_UnknownLabelValueFallsBack(t *testing.T) {
	t.Parallel()
	// A label value that no variant declares (e.g. a count the current
	// AllowedLocalSSDCounts table doesn't list anymore) must fall back to the
	// first Name-match rather than error, so reconciliation doesn't get stuck
	// on a single bad instance.
	its := []*karpcloudprovider.InstanceType{
		variantInstanceType("n2d-standard-8", "0"),
		variantInstanceType("n2d-standard-8", "2"),
	}
	got, ok := matchVariantForInstance(its, instanceWithSSDLabel("n2d-standard-8", "99"))
	require.True(t, ok)
	req := got.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount)
	require.Equal(t, "0", req.Any())
}

func TestMatchVariantForInstance_BundledSKUSingleVariant(t *testing.T) {
	t.Parallel()
	// Bundled-SSD families emit exactly one InstanceType per machine type.
	// The lookup still works for them — and must, since the GCE label is
	// stamped from that variant's requirement.
	its := []*karpcloudprovider.InstanceType{
		variantInstanceType("c4d-standard-8-lssd", "1"),
	}
	got, ok := matchVariantForInstance(its, instanceWithSSDLabel("c4d-standard-8-lssd", "1"))
	require.True(t, ok)
	require.Equal(t, "c4d-standard-8-lssd", got.Name)
}

func TestMatchVariantForInstance_NoNameMatch(t *testing.T) {
	t.Parallel()
	its := []*karpcloudprovider.InstanceType{
		variantInstanceType("n2d-standard-8", "0"),
	}
	got, ok := matchVariantForInstance(its, instanceWithSSDLabel("x9-standard-2", "0"))
	require.False(t, ok)
	require.Nil(t, got)
}

func TestRepairPolicies_NPDConditionsPolarity(t *testing.T) {
	t.Parallel()
	// GKE Node Problem Detector conditions use True=problem polarity (opposite of NodeReady).
	// NPD sets a condition to True when a problem is detected and omits it otherwise.
	// ConditionFalse would never match and ConditionTrue must be used to trigger repair.
	npdConditions := map[corev1.NodeConditionType]bool{
		"KernelDeadlock":            true,
		"ReadonlyFilesystem":        true,
		"FrequentKubeletRestart":    true,
		"FrequentContainerdRestart": true,
	}
	for _, p := range (&CloudProvider{}).RepairPolicies() {
		if npdConditions[p.ConditionType] {
			require.Equal(t, corev1.ConditionTrue, p.ConditionStatus,
				"NPD condition %s must use ConditionTrue polarity", p.ConditionType)
		}
	}
}
