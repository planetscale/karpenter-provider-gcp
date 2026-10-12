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
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
)

type recordingSubnetProvider struct {
	emptySubnetProvider
	invalidated [][2]string
}

func (p *recordingSubnetProvider) Invalidate(network, subnetwork string) {
	p.invalidated = append(p.invalidated, [2]string{network, subnetwork})
}

type recordingGKEProvider struct {
	fakeGKEProvider
	invalidations int
}

func (p *recordingGKEProvider) InvalidateClusterConfig() {
	p.invalidations++
}

func TestInsertInstanceWithPodRanges(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name                  string
		code                  string
		operationFailure      bool
		failAll               bool
		wantRetryable         bool
		wantUnavailable       bool
		wantSubnetInvalidates int
		wantInserts           int
	}{
		{name: "synchronous exhaustion then success", code: "IP_SPACE_EXHAUSTED", wantSubnetInvalidates: 1, wantInserts: 2},
		{name: "asynchronous exhaustion then success", code: "IP_SPACE_EXHAUSTED_WITH_DETAILS", operationFailure: true, wantSubnetInvalidates: 1, wantInserts: 2},
		{name: "all ranges exhausted synchronously", code: "IP_SPACE_EXHAUSTED", failAll: true, wantUnavailable: true, wantSubnetInvalidates: 2, wantInserts: 2},
		{name: "all ranges exhausted asynchronously", code: "IP_SPACE_EXHAUSTED_WITH_DETAILS", operationFailure: true, failAll: true, wantUnavailable: true, wantSubnetInvalidates: 2, wantInserts: 2},
		{name: "machine capacity is not a range failure", code: "ZONE_RESOURCE_POOL_EXHAUSTED", failAll: true, wantRetryable: true, wantUnavailable: true, wantInserts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var ranges []string
			p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/instances"):
					var instance compute.Instance
					if err := json.NewDecoder(r.Body).Decode(&instance); err != nil {
						t.Error(err)
						http.Error(w, "invalid instance", http.StatusBadRequest)
						return
					}
					ranges = append(ranges, instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName)
					if !tt.operationFailure && (len(ranges) == 1 || tt.failAll) {
						w.WriteHeader(http.StatusBadRequest)
						writeJSON(w, map[string]any{"error": map[string]any{"errors": []map[string]string{{"reason": tt.code}}}})
						return
					}
					writeJSON(w, &compute.Operation{Name: "insert-op"})
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/operations/insert-op"):
					op := &compute.Operation{Name: "insert-op", Status: "DONE"}
					if tt.operationFailure && (len(ranges) == 1 || tt.failAll) {
						op.Error = &compute.OperationError{Errors: []*compute.OperationErrorErrors{{Code: tt.code, Message: "allocation failed"}}}
					}
					writeJSON(w, op)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			c := cache.New(unavailableofferings.DefaultTTL, unavailableofferings.CleanupInterval)
			p.unavailableOfferings = unavailableofferings.NewUnavailableOfferingsWithCache(c)
			subnets := &recordingSubnetProvider{}
			gke := &recordingGKEProvider{}
			p.subnetProvider, p.gkeProvider = subnets, gke
			instance := &compute.Instance{Name: "karpenter-test", NetworkInterfaces: []*compute.NetworkInterface{{
				Network: "projects/test-project/global/networks/default", Subnetwork: "regions/us-central1/subnetworks/default",
				AliasIpRanges: []*compute.AliasIpRange{{}},
			}}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// Unknown utilization preserves discovery order. Exhaustion must still
			// fall back without marking the machine offering unavailable early.
			retryable, err := p.insertInstanceWithPodRanges(ctx, instance, makeNonGPUIT(), "us-central1-a", karpv1.CapacityTypeOnDemand, []string{"default-pods", "extra-pods"}, true)

			require.Equal(t, tt.wantRetryable, retryable)
			require.Equal(t, tt.wantUnavailable, cloudprovider.IsInsufficientCapacityError(err))
			if tt.wantUnavailable {
				requireStoredTTL(t, c, "n2-standard-4", "us-central1-a", karpv1.CapacityTypeOnDemand, insufficientCapacityBackoffTTL(insufficientCapacityDetails{code: tt.code}))
			} else {
				require.NoError(t, err)
				require.Empty(t, c.Items(), "successful fallback must not evict the offering")
			}
			mu.Lock()
			require.Equal(t, []string{"default-pods", "extra-pods"}[:tt.wantInserts], ranges)
			mu.Unlock()
			require.Len(t, subnets.invalidated, tt.wantSubnetInvalidates)
			for _, target := range subnets.invalidated {
				require.Equal(t, [2]string{instance.NetworkInterfaces[0].Network, instance.NetworkInterfaces[0].Subnetwork}, target)
			}
			require.Zero(t, gke.invalidations, "exhaustion invalidates utilization, not cluster range discovery")
		})
	}
}

func TestInsertInstanceWithRejectedPodRange(t *testing.T) {
	t.Parallel()

	for _, discovered := range []bool{true, false} {
		t.Run(map[bool]string{true: "discovered", false: "explicit"}[discovered], func(t *testing.T) {
			t.Parallel()
			p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"error": map[string]any{"errors": []map[string]string{{
					"reason": "invalidParameter", "message": "aliasIpRanges.subnetworkRangeName 'deleted-pods' does not exist",
				}}}})
			}))
			subnets, gke := &recordingSubnetProvider{}, &recordingGKEProvider{}
			p.subnetProvider, p.gkeProvider = subnets, gke
			c := cache.New(unavailableofferings.DefaultTTL, unavailableofferings.CleanupInterval)
			p.unavailableOfferings = unavailableofferings.NewUnavailableOfferingsWithCache(c)
			instance := &compute.Instance{NetworkInterfaces: []*compute.NetworkInterface{{
				Network: "projects/test-project/global/networks/default", Subnetwork: "regions/us-central1/subnetworks/default",
				AliasIpRanges: []*compute.AliasIpRange{{}},
			}}}

			retryable, err := p.insertInstanceWithPodRanges(context.Background(), instance, makeNonGPUIT(), "us-central1-a", karpv1.CapacityTypeOnDemand, []string{"deleted-pods"}, discovered)

			require.Error(t, err)
			require.True(t, retryable)
			require.Len(t, subnets.invalidated, 1)
			require.Equal(t, map[bool]int{true: 1, false: 0}[discovered], gke.invalidations)
			require.Empty(t, c.Items(), "range rejection is not machine unavailability")
		})
	}
}

func TestGetOrCreateInstanceRanksAdditionalPodRange(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var ranges []string
	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost:
			var instance compute.Instance
			if err := json.NewDecoder(r.Body).Decode(&instance); err != nil {
				t.Error(err)
				http.Error(w, "invalid instance", http.StatusBadRequest)
				return
			}
			ranges = append(ranges, instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName)
			writeJSON(w, &compute.Operation{Name: "insert-op"})
		case strings.HasSuffix(r.URL.Path, "/operations/insert-op"):
			writeJSON(w, &compute.Operation{Name: "insert-op", Status: "DONE"})
		default:
			http.NotFound(w, r)
		}
	}))
	p.computeDefaultSA = "123-compute@developer.gserviceaccount.com"
	cluster := makeCluster("projects/test-project/global/networks/default", "regions/us-central1/subnetworks/default", "default-pods", false)
	cluster.IpAllocationPolicy.AdditionalPodRangesConfig = &containerv1.AdditionalPodRangesConfig{PodRangeNames: []string{"extra-pods"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	instance, zone, retryable, err := p.getOrCreateInstance(ctx, spotOrOnDemandNodeClaim(), &v1alpha1.GCENodeClass{}, makeNonGPUIT(),
		makeSourceMetadata("max-pods-per-node=110"), cluster, "us-central1-a", karpv1.CapacityTypeOnDemand, nil, nil, 0,
		map[string]int64{"default-pods": 100, "extra-pods": 60000})

	require.NoError(t, err)
	require.False(t, retryable)
	require.Equal(t, "us-central1-a", zone)
	require.Equal(t, "extra-pods", instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"extra-pods"}, ranges, "launch must use the less-full additional range on its first attempt")
}
