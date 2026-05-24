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
	"errors"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// TestResolveLocalSSDCount pins the precedence used to advertise the local-SSD
// count to the GKE bootstrapper via kube-env. The NodeClaim's
// scheduling.Requirements is the primary input.
func TestResolveLocalSSDCount(t *testing.T) {
	bundled := func(count int32) *computepb.MachineType {
		return &computepb.MachineType{
			BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr(count)},
		}
	}
	nonBundled := &computepb.MachineType{}

	countReq := func(op corev1.NodeSelectorOperator, values ...string) scheduling.Requirements {
		return scheduling.NewRequirements(
			scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, op, values...),
		)
	}

	cases := []struct {
		name             string
		reqs             scheduling.Requirements
		nc               *v1alpha1.GCENodeClass
		mt               *computepb.MachineType
		instanceTypeName string
		want             int
		// One of these is non-empty when an error is expected:
		wantCreateReason string // expect *cloudprovider.CreateError with this ConditionReason
		wantRetryable    bool   // expect a non-CreateError (caller wraps as retryable)
	}{
		{
			name:             "count In:[\"4\"] on configurable IT → 4",
			reqs:             countReq(corev1.NodeSelectorOpIn, "4"),
			nc:               &v1alpha1.GCENodeClass{},
			mt:               nonBundled,
			instanceTypeName: "n2d-standard-8",
			want:             4,
		},
		{
			name:             "count In:[\"4\"] on bundled IT → bundled count (2) wins",
			reqs:             countReq(corev1.NodeSelectorOpIn, "4"),
			nc:               &v1alpha1.GCENodeClass{},
			mt:               bundled(2),
			instanceTypeName: "z3-highmem-22-standardlssd",
			want:             2,
		},
		{
			name:             "count Gt:0 → UnsupportedLocalSSDCountOperator CreateError",
			reqs:             countReq(corev1.NodeSelectorOpGt, "0"),
			nc:               &v1alpha1.GCENodeClass{},
			mt:               nonBundled,
			instanceTypeName: "n2d-standard-8",
			wantCreateReason: reasonUnsupportedLocalSSDCountOperator,
		},
		{
			name:             "count In:[\"2\",\"4\"] → MultiValuedLocalSSDCount CreateError",
			reqs:             countReq(corev1.NodeSelectorOpIn, "2", "4"),
			nc:               &v1alpha1.GCENodeClass{},
			mt:               nonBundled,
			instanceTypeName: "n2d-standard-8",
			wantCreateReason: reasonMultiValuedLocalSSDCount,
		},
		{
			name:             "no count req + configurable IT → 0",
			reqs:             scheduling.NewRequirements(),
			nc:               &v1alpha1.GCENodeClass{},
			mt:               nonBundled,
			instanceTypeName: "n2d-standard-8",
			want:             0,
		},
		{
			name:             "no count req + bundled IT → bundled count",
			reqs:             scheduling.NewRequirements(),
			nc:               &v1alpha1.GCENodeClass{},
			mt:               bundled(2),
			instanceTypeName: "z3-highmem-22-standardlssd",
			want:             2,
		},
		{
			name:             "cache miss + bundled name (mt nil) → retryable error",
			reqs:             scheduling.NewRequirements(),
			nc:               &v1alpha1.GCENodeClass{},
			mt:               nil,
			instanceTypeName: "c4d-standard-8-lssd",
			wantRetryable:    true,
		},
		// Transitional fallback: legacy disks[].category=local-ssd. Kept so
		// existing NodeClasses continue to provision while disks[] is retired.
		{
			name: "no count req + legacy disks[] → counted from disks[]",
			reqs: scheduling.NewRequirements(),
			nc: &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{Disks: []v1alpha1.Disk{
				{Category: "local-ssd"},
				{Category: "local-ssd"},
			}}},
			mt:               nonBundled,
			instanceTypeName: "n2-standard-8",
			want:             2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveLocalSSDCount(tc.reqs, tc.nc, tc.instanceTypeName, tc.mt)
			switch {
			case tc.wantCreateReason != "":
				require.Error(t, err)
				var ce *cloudprovider.CreateError
				require.ErrorAs(t, err, &ce, "expected *cloudprovider.CreateError, got %T", err)
				assert.Equal(t, tc.wantCreateReason, ce.ConditionReason)
			case tc.wantRetryable:
				require.Error(t, err)
				var ce *cloudprovider.CreateError
				assert.False(t, errors.As(err, &ce), "expected non-CreateError (retryable), got CreateError")
			default:
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
		})
	}
}
