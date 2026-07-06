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
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/localssd"
)

const (
	maxInstanceTypes               = 20
	maxNodeCIDR                    = 23
	instanceCacheExpiration        = 15 * time.Second
	zoneOperationPollInterval      = 1 * time.Second
	defaultZoneOperationTimeout    = 2 * time.Minute
	ipSpaceInsufficientCapacityTTL = 30 * time.Second

	instanceTerminationActionDelete = "DELETE"
)

var InsufficientCapacityErrorCodes = sets.NewString(
	"ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS",
	"ZONE_RESOURCE_POOL_EXHAUSTED",
	"IP_SPACE_EXHAUSTED_WITH_DETAILS",
	"IP_SPACE_EXHAUSTED",
)

type Provider interface {
	Create(context.Context, *v1alpha1.GCENodeClass, *karpv1.NodeClaim, []*cloudprovider.InstanceType) (*Instance, error)
	Get(context.Context, string) (*Instance, error)
	List(context.Context) ([]*Instance, error)
	Delete(context.Context, string) error
	CreateTags(context.Context, string, map[string]string) error
}

type DefaultProvider struct {
	gkeProvider              gke.Provider
	instanceTypeProvider     instancetype.Provider
	nodePoolTemplateProvider nodepooltemplate.Provider
	versionProvider          version.Provider
	unavailableOfferings     *pkgcache.UnavailableOfferings

	// In current implementation, instanceID == InstanceName
	instanceCache *cache.Cache

	clusterName           string
	clusterLocation       string
	region                string
	projectID             string
	defaultServiceAccount string
	computeDefaultSA      string
	computeService        *compute.Service
}

func NewProvider(clusterName, clusterLocation, region, projectID, defaultServiceAccount, computeDefaultSA string,
	computeService *compute.Service,
	gkeProvider gke.Provider,
	instanceTypeProvider instancetype.Provider,
	nodePoolTemplateProvider nodepooltemplate.Provider,
	versionProvider version.Provider,
	unavailableOfferings *pkgcache.UnavailableOfferings,
) Provider {
	return &DefaultProvider{
		gkeProvider:              gkeProvider,
		instanceTypeProvider:     instanceTypeProvider,
		nodePoolTemplateProvider: nodePoolTemplateProvider,
		versionProvider:          versionProvider,
		unavailableOfferings:     unavailableOfferings,
		instanceCache:            cache.New(instanceCacheExpiration, instanceCacheExpiration),
		clusterName:              clusterName,
		clusterLocation:          clusterLocation,
		region:                   region,
		projectID:                projectID,
		defaultServiceAccount:    defaultServiceAccount,
		computeDefaultSA:         computeDefaultSA,
		computeService:           computeService,
	}
}

func (p *DefaultProvider) waitOperationDone(ctx context.Context,
	instanceType, zone, capacityType, operationName string,
) error {
	waitCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		waitCtx, cancel = context.WithTimeout(ctx, defaultZoneOperationTimeout)
	}
	defer cancel()

	ticker := time.NewTicker(zoneOperationPollInterval)
	defer ticker.Stop()

	for {
		op, err := p.computeService.ZoneOperations.Get(p.projectID, zone, operationName).Context(waitCtx).Do()
		if err != nil {
			if waitCtx.Err() != nil {
				return fmt.Errorf("waiting for operation %s: %w", operationName, waitCtx.Err())
			}
			if isTransientError(err) {
				log.FromContext(ctx).V(1).Info("transient error polling operation, retrying",
					"operation", operationName, "zone", zone, "error", err)
				if tickErr := waitForNextTick(waitCtx, ticker); tickErr != nil {
					return fmt.Errorf("waiting for operation %s: %w", operationName, tickErr)
				}
				continue
			}
			return fmt.Errorf("getting operation: %w", err)
		}

		if op.Status == "DONE" {
			if op.Error != nil {
				return p.handleZoneOperationError(waitCtx, op, instanceType, zone, capacityType)
			}
			return nil
		}

		if err := waitForNextTick(waitCtx, ticker); err != nil {
			return fmt.Errorf("waiting for operation %s: %w", operationName, err)
		}
	}
}

func waitForNextTick(ctx context.Context, ticker *time.Ticker) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ticker.C:
		return nil
	}
}

func (p *DefaultProvider) handleZoneOperationError(ctx context.Context, op *compute.Operation, instanceType, zone, capacityType string) error {
	capacityError, found := lo.Find(op.Error.Errors, func(e *compute.OperationErrorErrors) bool {
		return isInsufficientCapacityError(e)
	})
	if found {
		reason := capacityError.Message
		if reason == "" {
			reason = capacityError.Code
		}
		ttl := insufficientCapacityBackoffTTL(capacityError.Code)
		p.unavailableOfferings.MarkUnavailableWithTTL(ctx, reason, instanceType, zone, capacityType, ttl)
		return cloudprovider.NewInsufficientCapacityError(fmt.Errorf("zone %s insufficient capacity: %s", zone, reason))
	}

	errorMsgs := lo.Map(op.Error.Errors, func(e *compute.OperationErrorErrors, _ int) string {
		return e.Message
	})
	return fmt.Errorf("operation failed: %s", strings.Join(errorMsgs, "; "))
}

func isTransientError(err error) bool {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code >= 500
	}
	return false
}

func isInsufficientCapacityError(operationError *compute.OperationErrorErrors) bool {
	if operationError == nil {
		return false
	}

	return InsufficientCapacityErrorCodes.Has(operationError.Code)
}

func extractInsertInsufficientCapacityReason(err error) (string, string, bool) {
	var apiError *googleapi.Error
	if !errors.As(err, &apiError) {
		return "", "", false
	}

	for _, detail := range apiError.Errors {
		if InsufficientCapacityErrorCodes.Has(detail.Reason) {
			reason := detail.Message
			if reason == "" {
				reason = detail.Reason
			}
			return reason, detail.Reason, true
		}
	}

	return "", "", false
}

func insufficientCapacityBackoffTTL(reasonCode string) time.Duration {
	if reasonCode == "IP_SPACE_EXHAUSTED_WITH_DETAILS" || reasonCode == "IP_SPACE_EXHAUSTED" {
		return ipSpaceInsufficientCapacityTTL
	}

	return pkgcache.UnavailableOfferingsTTL
}

func (p *DefaultProvider) isInstanceExists(ctx context.Context, zone, instanceName string) (*compute.Instance, bool, error) {
	instance, err := p.computeService.Instances.Get(p.projectID, zone, instanceName).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get instance: %w", err)
	}
	return instance, true, nil
}

func (p *DefaultProvider) findInstanceByNodeClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*compute.Instance, error) {
	instanceName := fmt.Sprintf("karpenter-%s", nodeClaim.Name)
	call := p.computeService.Instances.AggregatedList(p.projectID).Filter(fmt.Sprintf("name eq %s", instanceName))

	regionPrefix := "zones/" + p.region + "-"
	var instance *compute.Instance
	err := call.Pages(ctx, func(resp *compute.InstanceAggregatedList) error {
		for zoneKey, items := range resp.Items {
			if !strings.HasPrefix(zoneKey, regionPrefix) {
				continue
			}
			for _, i := range items.Instances {
				instance = i
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return instance, nil
}

func (p *DefaultProvider) adoptExistingInstance(ctx context.Context, existingInstance *compute.Instance, capacityType string) *Instance {
	zone := existingInstance.Zone
	if split := strings.Split(zone, "/"); len(split) > 0 {
		zone = split[len(split)-1]
	}
	machineType := existingInstance.MachineType
	if split := strings.Split(machineType, "/"); len(split) > 0 {
		machineType = split[len(split)-1]
	}

	log.FromContext(ctx).Info("Found existing instance for NodeClaim", "instance", existingInstance.Name, "zone", zone)
	return &Instance{
		InstanceID:   existingInstance.Name,
		Name:         existingInstance.Name,
		Type:         machineType,
		Location:     zone,
		ProjectID:    p.projectID,
		ImageID:      resolveInstanceImage(existingInstance),
		CreationTime: parseCreationTime(existingInstance.CreationTimestamp),
		CapacityType: capacityType,
		Labels:       existingInstance.Labels,
		Tags:         existingInstance.Labels,
		Status:       existingInstance.Status,
	}
}

func (p *DefaultProvider) Create(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*Instance, error) {
	if len(instanceTypes) == 0 {
		return nil, fmt.Errorf("no instance types provided")
	}

	warnLegacyLocalSSDDisks(ctx, nodeClass)

	instanceTypes = orderInstanceTypesByPrice(instanceTypes, scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...))
	capacityType := p.getCapacityType(nodeClaim, instanceTypes)

	// Check if the instance already exists in any zone
	if existingInstance, err := p.findInstanceByNodeClaim(ctx, nodeClaim); err != nil {
		log.FromContext(ctx).Error(err, "failed to check if instance exists in region", "nodeClaim", nodeClaim.Name)
	} else if existingInstance != nil {
		return p.adoptExistingInstance(ctx, existingInstance, capacityType), nil
	}

	var errs []error
	// try all instance types, if one is available, use it
	for _, instanceType := range instanceTypes {
		instance, zone, template, err := p.tryCreateInstance(ctx, nodeClass, nodeClaim, instanceType, capacityType)
		if err != nil {
			var retryableErr *retryableError
			if errors.As(err, &retryableErr) {
				errs = append(errs, retryableErr.err)
				continue
			}
			return nil, err
		}

		// we could wait for the node to be present in kubernetes api via csr sign up
		// should be done with watcher, for now implemented as a csr controller
		log.FromContext(ctx).Info("Created instance", "instanceName", instance.Name, "instanceType", instanceType.Name,
			"zone", zone, "projectID", p.projectID, "region", p.region, "providerID", instance.Name, "providerID", instance.Name,
			"Labels", instance.Labels, "Tags", instance.Tags, "Status", instance.Status)

		return &Instance{
			InstanceID: instance.Name,
			Name:       instance.Name,
			// Refer to https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/45#discussion_r2115586327
			// In this develop period, we are using a static instance type to avoid high cost of creating a new instance type for each node claim.
			// Type:         instanceType.Name,
			Type:         instanceType.Name,
			Location:     zone,
			ProjectID:    p.projectID,
			ImageID:      resolveInstanceImage(instance),
			CreationTime: time.Now(),
			CapacityType: capacityType,
			Tags:         template.Properties.Labels,
			Labels:       instance.Labels,
			Status:       InstanceStatusProvisioning,
		}, nil
	}

	if len(errs) == 0 {
		return nil, fmt.Errorf("failed to create instance after trying all instance types: unknown error")
	}

	joined := errors.Join(errs...)
	if lo.SomeBy(errs, func(err error) bool { return cloudprovider.IsInsufficientCapacityError(err) }) {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("failed to create instance after trying all instance types: %w", joined))
	}

	return nil, fmt.Errorf("failed to create instance after trying all instance types: %w", joined)
}

type retryableError struct {
	err error
}

func (e *retryableError) Error() string {
	return e.err.Error()
}

func (p *DefaultProvider) tryCreateInstance(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceType *cloudprovider.InstanceType, capacityType string) (*compute.Instance, string, *compute.InstanceTemplate, error) {
	mt := p.instanceTypeProvider.GetMachineType(instanceType.Name)
	// evaluateLocalSSDConflict tolerates a nil mt (falls back to the name-suffix
	// predicate) because a conflict between user config and a name-recognized
	// bundled family is always a config bug, not a cache state issue.
	if err := evaluateLocalSSDConflict(nodeClass, instanceType.Name, mt); err != nil {
		log.FromContext(ctx).Error(err, "local-SSD conflict; skipping candidate", "instanceType", instanceType.Name)
		return nil, "", nil, &retryableError{err}
	}

	// resolveLocalSSDCount returns one of:
	//   - a *cloudprovider.CreateError (fatal: unsupported operator or
	//     multi-valued In on the count requirement) — surface as-is so the
	//     NodeClaim records Launched=False with a meaningful reason;
	//   - a plain error (retryable: cache miss on a bundled-name SKU; we
	//     can't pick an authoritative count, so fall through to the next
	//     candidate instead of booting a node whose SSDs would never be
	//     formatted by the GKE bootstrapper);
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	ssdCount, err := resolveLocalSSDCount(reqs, nodeClass, instanceType.Name, mt)
	if err != nil {
		var createErr *cloudprovider.CreateError
		if errors.As(err, &createErr) {
			log.FromContext(ctx).Error(err, "resolveLocalSSDCount failed; surfacing as CreateError", "instanceType", instanceType.Name, "reason", createErr.ConditionReason)
			return nil, "", nil, err
		}
		log.FromContext(ctx).Error(err, "resolveLocalSSDCount failed; treating as retryable", "instanceType", instanceType.Name)
		return nil, "", nil, &retryableError{err}
	}

	zone, err := p.selectZone(ctx, nodeClaim, instanceType, capacityType)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to select zone for instance type", "instanceType", instanceType.Name)
		return nil, "", nil, &retryableError{err}
	}

	sourcePoolName, err := p.nodePoolTemplateProvider.GetSourcePoolName(ctx)
	if err != nil {
		return nil, "", nil, &retryableError{fmt.Errorf("getting source pool name: %w", err)}
	}

	template, err := p.findTemplateByNodePoolName(ctx, sourcePoolName)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to find template for source pool", "pool", sourcePoolName)
		return nil, "", nil, &retryableError{err}
	}

	clusterConfig, err := p.gkeProvider.GetClusterConfig(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to fetch cluster config")
		return nil, "", nil, &retryableError{err}
	}

	instance, retryable, err := p.getOrCreateInstance(ctx, nodeClaim, nodeClass, instanceType, template, clusterConfig, sourcePoolName, zone, capacityType, mt, ssdCount)
	if err != nil {
		if retryable {
			return nil, "", nil, &retryableError{err}
		}
		return nil, "", nil, err
	}

	return instance, zone, template, nil
}

func (p *DefaultProvider) getOrCreateInstance(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, template *compute.InstanceTemplate, clusterConfig *container.Cluster, nodePoolName, zone, capacityType string, mt *computepb.MachineType, ssdCount int) (*compute.Instance, bool, error) {
	instanceName := fmt.Sprintf("karpenter-%s", nodeClaim.Name)
	instance, exists, err := p.isInstanceExists(ctx, zone, instanceName)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to check if instance exists", "instanceName", instanceName)
		return nil, false, fmt.Errorf("failed to check if instance exists: %w", err)
	}

	if exists {
		return instance, false, nil
	}

	instance, err = p.buildInstance(ctx, nodeClaim, nodeClass, instanceType, template, clusterConfig, nodePoolName, zone, instanceName, capacityType, mt, ssdCount)
	if err != nil {
		return nil, false, fmt.Errorf("building instance %s: %w", instanceName, err)
	}
	op, err := p.computeService.Instances.Insert(p.projectID, zone, instance).Context(ctx).Do()
	if err != nil {
		reason, reasonCode, insufficient := extractInsertInsufficientCapacityReason(err)
		if insufficient {
			if reason == "" {
				reason = "insufficient capacity"
			}
			ttl := insufficientCapacityBackoffTTL(reasonCode)
			p.unavailableOfferings.MarkUnavailableWithTTL(ctx, reason, instanceType.Name, zone, capacityType, ttl)
			err = cloudprovider.NewInsufficientCapacityError(fmt.Errorf("zone %s insufficient capacity: %s", zone, reason))

			// If IP space is exhausted, trying other instance types won't help as they share the same subnet.
			// We should fail fast to avoid unnecessary API calls and noise.
			if reasonCode == "IP_SPACE_EXHAUSTED" || reasonCode == "IP_SPACE_EXHAUSTED_WITH_DETAILS" {
				return nil, false, err
			}
		}
		log.FromContext(ctx).Error(err, "failed to create instance", "instanceType", instanceType.Name, "zone", zone)
		return nil, true, err
	}

	if err := p.waitOperationDone(ctx, instanceType.Name, zone, capacityType, op.Name); err != nil {
		log.FromContext(ctx).Error(err, "failed to wait for operation to be done", "instanceType", instanceType.Name, "zone", zone)
		return nil, true, err
	}

	return instance, false, nil
}

func resolveInstanceImage(instance *compute.Instance) string {
	image, ok := lo.Find(instance.Disks, func(disk *compute.AttachedDisk) bool {
		return disk.Boot
	})
	if !ok {
		return ""
	}
	if image.InitializeParams == nil {
		return ""
	}
	return image.InitializeParams.SourceImage
}

// getCapacityType selects spot if both constraints are flexible and there is an
// available offering.
func (p *DefaultProvider) getCapacityType(nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) string {
	requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	if requirements.Get(karpv1.CapacityTypeLabelKey).Has(karpv1.CapacityTypeSpot) {
		requirements[karpv1.CapacityTypeLabelKey] = scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot)
		for _, instanceType := range instanceTypes {
			for _, offering := range instanceType.Offerings.Available() {
				if requirements.Compatible(offering.Requirements, scheduling.AllowUndefinedWellKnownLabels) == nil {
					return karpv1.CapacityTypeSpot
				}
			}
		}
	}
	return karpv1.CapacityTypeOnDemand
}

func orderInstanceTypesByPrice(instanceTypes []*cloudprovider.InstanceType, requirements scheduling.Requirements) []*cloudprovider.InstanceType {
	// Order instance types so that we get the cheapest instance types of the available offerings.
	// Variants of the same configurable-SSD machine type share Name and price; the SSD-count
	// tie-break makes the variant choice deterministic (smallest count wins), so a no-SSD-count
	// pod consistently lands on the count=0 variant and an Ephemeral capacity request consistently
	// picks the smallest-count variant that satisfies.
	sort.Slice(instanceTypes, func(i, j int) bool {
		iPrice := math.MaxFloat64
		jPrice := math.MaxFloat64
		if len(instanceTypes[i].Offerings.Available().Compatible(requirements)) > 0 {
			iPrice = instanceTypes[i].Offerings.Available().Compatible(requirements).Cheapest().Price
		}
		if len(instanceTypes[j].Offerings.Available().Compatible(requirements)) > 0 {
			jPrice = instanceTypes[j].Offerings.Available().Compatible(requirements).Cheapest().Price
		}
		if iPrice != jPrice {
			return iPrice < jPrice
		}
		if instanceTypes[i].Name != instanceTypes[j].Name {
			return instanceTypes[i].Name < instanceTypes[j].Name
		}
		return variantSSDCount(instanceTypes[i]) < variantSSDCount(instanceTypes[j])
	})
	return instanceTypes
}

// variantSSDCount returns the local-SSD count pinned on its
// karpenter.k8s.gcp/instance-local-ssd-count requirement. Returns 0 when the
// requirement is absent or has a non-integer value (shouldn't happen — every
// variant in instancetype.List sets it via localSSDCountRequirement).
func variantSSDCount(it *cloudprovider.InstanceType) int {
	req := it.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount)
	if len(req.Values()) == 0 {
		return 0
	}
	n, err := strconv.Atoi(req.Values()[0])
	if err != nil {
		return 0
	}
	return n
}

func filterZonesByRequirement(zones []string, reqs scheduling.Requirements) []string {
	zoneReq := reqs.Get(corev1.LabelTopologyZone)
	if zoneReq == nil || len(zoneReq.Values()) == 0 {
		return zones
	}
	allowed := sets.NewString()
	for _, z := range zones {
		if lo.Contains(zoneReq.Values(), z) {
			allowed.Insert(z)
		}
	}
	return allowed.List()
}

func randomZone(zones []string) string {
	randomIndex, _ := rand.Int(rand.Reader, big.NewInt(int64(len(zones))))
	return zones[randomIndex.Int64()]
}

func cheapestCompatibleZone(zones []string, reqs scheduling.Requirements, offerings cloudprovider.Offerings) string {
	cheapestZone := ""
	cheapestPrice := math.MaxFloat64
	zonesSet := sets.NewString(zones...)
	for _, offering := range offerings {
		if reqs.Compatible(offering.Requirements, scheduling.AllowUndefinedWellKnownLabels) != nil {
			continue
		}
		zone := offering.Requirements.Get(corev1.LabelTopologyZone).Any()
		if !zonesSet.Has(zone) {
			continue
		}
		if offering.Price < cheapestPrice {
			cheapestZone = zone
			cheapestPrice = offering.Price
		}
	}
	return cheapestZone
}

func (p *DefaultProvider) selectZone(ctx context.Context, nodeClaim *karpv1.NodeClaim,
	instanceType *cloudprovider.InstanceType, capacityType string,
) (string, error) {
	zones, err := p.gkeProvider.ResolveClusterZones(ctx)
	if err != nil {
		return "", err
	}

	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	zones = filterZonesByRequirement(zones, reqs)

	// If no zones remain after applying requirements, fail fast.
	if len(zones) == 0 {
		return "", fmt.Errorf("no zones match topology requirement %q", corev1.LabelTopologyZone)
	}

	// Skip zones with known ICE for this instance type and capacity type.
	// Without this filter, every retry calls MarkUnavailable and resets the
	// 30-min TTL, preventing natural expiry. Applied to both on-demand and
	// spot so both paths behave consistently.
	zones = lo.Filter(zones, func(z string, _ int) bool {
		return !p.unavailableOfferings.IsUnavailable(instanceType.Name, z, capacityType)
	})
	if len(zones) == 0 {
		return "", cloudprovider.NewInsufficientCapacityError(
			fmt.Errorf("all zones exhausted for instance type %s", instanceType.Name))
	}

	// For on-demand, randomly select a zone from those that satisfy the requirement,
	// to spread load while still honoring topology constraints.
	if capacityType == karpv1.CapacityTypeOnDemand {
		return randomZone(zones), nil
	}
	// else for spot, choose the cheapest zone
	return cheapestCompatibleZone(zones, reqs, instanceType.Offerings), nil
}

//nolint:gocyclo
func (p *DefaultProvider) findTemplateByNodePoolName(ctx context.Context, nodePoolName string) (*compute.InstanceTemplate, error) {
	if nodePoolName == "" {
		return nil, fmt.Errorf("node pool name not specified in ImageSelectorTerm")
	}

	instanceTemplates, err := p.computeService.RegionInstanceTemplates.List(p.projectID, p.region).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("cannot list all instance templates for node pool name %q: %w", nodePoolName, err)
	}

	for _, t := range instanceTemplates.Items {
		if t.Properties != nil && t.Properties.Labels != nil {
			// instanceTemplates are shared across clusters, so we need to check if the template belongs to the current cluster
			// This is done by checking the metadata for the cluster name label.
			if t.Properties.Metadata != nil {
				metadataClusterNamemetadata, err := metadata.GetClusterName(t.Properties.Metadata)
				if err != nil {
					log.FromContext(ctx).Error(err, "failed to get cluster name from metadata", "instanceTemplate", t.Name)
					continue
				}
				if metadataClusterNamemetadata != p.clusterName {
					continue
				}
			}
			if val, ok := t.Properties.Labels["goog-k8s-node-pool-name"]; ok && val == nodePoolName {
				log.FromContext(ctx).Info("Found instance template", "templateName", t.Name, "clusterName", p.clusterName)
				return t, nil
			}
		}
	}

	return nil, fmt.Errorf("no instance template found with label goog-k8s-node-pool-name=%s", nodePoolName)
}

// renderDiskProperties builds the AttachedDisks list for the new instance.
// ssdCount is the resolved local-SSD partition count (from resolveLocalSSDCount
// — either the bundled count, the NodeClaim's count requirement, or 0); the
// renderer attaches exactly that many SCRATCH disks.
//
// Legacy disks[].category=local-ssd entries are skipped here: the count is
// authoritative from the scheduler / requirement path, and emitting one
// SCRATCH per entry on top of ssdCount would double-attach.
func (p *DefaultProvider) renderDiskProperties(instanceType *cloudprovider.InstanceType,
	nodeClass *v1alpha1.GCENodeClass, zone string, ssdCount int, mt *computepb.MachineType,
) ([]*compute.AttachedDisk, error) {
	disks := nodeClass.Spec.Disks
	sort.Slice(disks, func(i, j int) bool {
		return disks[i].Boot
	})

	attachedDisks := make([]*compute.AttachedDisk, 0, len(disks)+ssdCount)
	for _, disk := range disks {
		if disk.Category == "local-ssd" {
			continue
		}
		// Create a new disk configuration for each disk to avoid sharing references
		initParams := &compute.AttachedDiskInitializeParams{
			DiskType:   fmt.Sprintf("projects/%s/zones/%s/diskTypes/%s", p.projectID, zone, disk.Category),
			DiskSizeGb: int64(disk.SizeGiB),
		}
		if disk.ProvisionedIOPS != nil {
			initParams.ProvisionedIops = *disk.ProvisionedIOPS
		}
		if disk.ProvisionedThroughput != nil {
			initParams.ProvisionedThroughput = *disk.ProvisionedThroughput
		}
		attachedDisk := &compute.AttachedDisk{
			AutoDelete:       true,
			Boot:             disk.Boot,
			InitializeParams: initParams,
		}

		requirements := instanceType.Requirements
		if requirements.Get(corev1.LabelArchStable).Has(imagefamily.OSArchARM64Requirement) {
			attachedDisk.InitializeParams.Architecture = imagefamily.OSArchitectureARM
		}

		if disk.Boot {
			targetImage, found := lo.Find(nodeClass.Status.Images, func(image v1alpha1.Image) bool {
				reqs := scheduling.NewNodeSelectorRequirements(image.Requirements...)
				return reqs.Compatible(instanceType.Requirements, scheduling.AllowUndefinedWellKnownLabels) == nil
			})
			if !found {
				return nil, fmt.Errorf("no target image found for node class %s", nodeClass.Name)
			}
			attachedDisk.InitializeParams.SourceImage = targetImage.SourceImage
		} else {
			attachedDisk.DeviceName = metadata.GetSecondaryDiskImageDeviceName(disk.SecondaryBootImage)
			attachedDisk.InitializeParams.SourceImage = disk.SecondaryBootImage
		}

		// Add disk encryption key if specified
		if disk.KMSKeyName != "" {
			attachedDisk.DiskEncryptionKey = &compute.CustomerEncryptionKey{
				KmsKeyName:           disk.KMSKeyName,
				KmsKeyServiceAccount: disk.KMSKeyServiceAccount,
			}
		}

		attachedDisks = append(attachedDisks, attachedDisk)
	}

	// Bundled-SSD families (C3/C4*/Z3/A3/A4*) auto-attach their local SSDs by
	// machine shape. An explicit SCRATCH disk is redundant for the C-series and
	// rejected outright for Z3 Titanium ("configuration_availability"), so emit
	// scratch disks only for configurable families that require explicit attach.
	if !hasBundledLocalSSDs(instanceType.Name, mt) {
		for i := 0; i < ssdCount; i++ {
			attachedDisks = append(attachedDisks, p.scratchDisk(zone))
		}
	}

	return attachedDisks, nil
}

// scratchDisk builds a local-SSD SCRATCH AttachedDisk. GCE rejects PERSISTENT
// for local-ssd, the partition size is family-defined (DiskSizeGb must be
// unset), and these disks don't take a SourceImage / DeviceName.
func (p *DefaultProvider) scratchDisk(zone string) *compute.AttachedDisk {
	return &compute.AttachedDisk{
		Type:       "SCRATCH",
		AutoDelete: true,
		Boot:       false,
		Interface:  "NVME",
		InitializeParams: &compute.AttachedDiskInitializeParams{
			DiskType: fmt.Sprintf("projects/%s/zones/%s/diskTypes/local-ssd", p.projectID, zone),
		},
	}
}

func (p *DefaultProvider) buildInstance(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, template *compute.InstanceTemplate, clusterConfig *container.Cluster, nodePoolName, zone, instanceName, capacityType string, mt *computepb.MachineType, ssdCount int) (*compute.Instance, error) {
	attachedDisks, err := p.renderDiskProperties(instanceType, nodeClass, zone, ssdCount, mt)
	if err != nil {
		return nil, fmt.Errorf("rendering disk properties: %w", err)
	}

	isGPUInstance := len(template.Properties.GuestAccelerators) > 0 || instanceTypeHasGPU(instanceType)

	// Setup metadata
	if err := p.setupInstanceMetadata(ctx, template.Properties.Metadata, nodeClass, instanceType, nodeClaim, nodePoolName, capacityType, isGPUInstance, ssdCount); err != nil {
		return nil, fmt.Errorf("setting up instance metadata: %w", err)
	}

	serviceAccounts, err := p.setupServiceAccounts(nodeClass)
	if err != nil {
		return nil, fmt.Errorf("setting up service accounts: %w", err)
	}

	instance := &compute.Instance{
		Name:              instanceName,
		MachineType:       fmt.Sprintf("zones/%s/machineTypes/%s", zone, instanceType.Name),
		Disks:             attachedDisks,
		NetworkInterfaces: p.setupNetworkInterfaces(clusterConfig, nodeClass),
		ServiceAccounts:   serviceAccounts,
		Metadata:          template.Properties.Metadata,
		Labels:            p.initializeInstanceLabels(nodeClass),
		Scheduling:        setupScheduling(capacityType),
		Tags:              buildInstanceTags(p.clusterName, clusterConfig.Id, nodeClass.Spec.NetworkTags),
	}

	// Configure Shielded VM options
	if nodeClass.Spec.ShieldedInstanceConfig != nil {
		sic := nodeClass.Spec.ShieldedInstanceConfig
		instance.ShieldedInstanceConfig = &compute.ShieldedInstanceConfig{}
		if sic.EnableSecureBoot != nil {
			instance.ShieldedInstanceConfig.EnableSecureBoot = *sic.EnableSecureBoot
			instance.ShieldedInstanceConfig.ForceSendFields = append(instance.ShieldedInstanceConfig.ForceSendFields, "EnableSecureBoot")
		}
		if sic.EnableVtpm != nil {
			instance.ShieldedInstanceConfig.EnableVtpm = *sic.EnableVtpm
			instance.ShieldedInstanceConfig.ForceSendFields = append(instance.ShieldedInstanceConfig.ForceSendFields, "EnableVtpm")
		}
		if sic.EnableIntegrityMonitoring != nil {
			instance.ShieldedInstanceConfig.EnableIntegrityMonitoring = *sic.EnableIntegrityMonitoring
			instance.ShieldedInstanceConfig.ForceSendFields = append(instance.ShieldedInstanceConfig.ForceSendFields, "EnableIntegrityMonitoring")
		}
	}

	// Configure capacity provision
	p.configureInstanceCapacityProvision(instance, capacityType)

	if policy := onHostMaintenancePolicy(instanceType, capacityType, mt); policy != "" {
		instance.Scheduling.OnHostMaintenance = policy
	}

	// Setup karpenter built-in labels
	p.setupInstanceLabels(instance, nodeClaim, nodeClass, instanceType)

	return instance, nil
}

func (p *DefaultProvider) setupNetworkInterfaces(cluster *container.Cluster, nodeClass *v1alpha1.GCENodeClass) []*compute.NetworkInterface {
	if cluster.NetworkConfig == nil {
		return nil
	}
	targetRange := podCIDRRange(nodeClass.GetMaxPods())
	clusterPrivate := cluster.PrivateClusterConfig != nil && cluster.PrivateClusterConfig.EnablePrivateNodes

	// Pod CIDR range name: NodeClass → cluster IpAllocationPolicy → let GKE pick.
	rangeName := ""
	if nodeClass.Spec.SubnetRangeName != nil {
		rangeName = *nodeClass.Spec.SubnetRangeName
	} else if cluster.IpAllocationPolicy != nil {
		rangeName = cluster.IpAllocationPolicy.ClusterSecondaryRangeName
	}

	// Primary interface: built from cluster config, overrideable via NodeClass networkConfig.
	subnetwork := cluster.NetworkConfig.Subnetwork
	disableExternal := clusterPrivate
	if nodeClass.Spec.NetworkConfig != nil {
		cfg := nodeClass.Spec.NetworkConfig
		if cfg.Subnetwork != "" {
			subnetwork = cfg.Subnetwork
		}
		if cfg.EnablePrivateNodes != nil {
			disableExternal = *cfg.EnablePrivateNodes
		}
	}

	primary := &compute.NetworkInterface{
		Network:    cluster.NetworkConfig.Network,
		Subnetwork: subnetwork,
		AliasIpRanges: []*compute.AliasIpRange{{
			IpCidrRange:         fmt.Sprintf("/%d", targetRange),
			SubnetworkRangeName: rangeName,
		}},
	}
	applyAccessConfig(primary, disableExternal)

	ifaces := []*compute.NetworkInterface{primary}

	if nodeClass.Spec.NetworkConfig != nil {
		ifaces = append(ifaces, buildAdditionalInterfaces(cluster.NetworkConfig.Network, nodeClass.Spec.NetworkConfig.AdditionalNetworkInterfaces, disableExternal)...)
	}

	return ifaces
}

// applyAccessConfig sets the ONE_TO_ONE_NAT access config on iface when external is enabled,
// or force-sends an empty slice so the GCP API does not insert it automatically.
func applyAccessConfig(iface *compute.NetworkInterface, disableExternal bool) {
	if !disableExternal {
		iface.AccessConfigs = []*compute.AccessConfig{{Type: "ONE_TO_ONE_NAT", Name: "External NAT"}}
	} else {
		iface.ForceSendFields = []string{"AccessConfigs"}
	}
}

// buildAdditionalInterfaces builds secondary NetworkInterface objects from additionalNetworkInterfaces.
// Subnetwork is required by the CRD schema on each entry. Network falls back to the cluster network
// when not explicitly set, mirroring GKE's additional_node_network_configs behavior.
func buildAdditionalInterfaces(clusterNetwork string, overrides []v1alpha1.AdditionalNetworkInterface, disableExternal bool) []*compute.NetworkInterface {
	ifaces := make([]*compute.NetworkInterface, 0, len(overrides))
	for _, override := range overrides {
		network := clusterNetwork
		if override.Network != "" {
			network = override.Network
		}
		iface := &compute.NetworkInterface{
			Network:    network,
			Subnetwork: override.Subnetwork,
		}
		applyAccessConfig(iface, disableExternal)
		ifaces = append(ifaces, iface)
	}
	return ifaces
}

// podCIDRRange returns the /N alias IP range size for the given maxPods count,
// following https://cloud.google.com/kubernetes-engine/docs/how-to/flexible-pod-cidr
func podCIDRRange(maxPods int32) int32 {
	switch {
	case maxPods <= 8:
		return 28
	case maxPods <= 16:
		return 27
	case maxPods <= 32:
		return 26
	case maxPods <= 64:
		return 25
	case maxPods <= 128:
		return 24
	default:
		return int32(maxNodeCIDR)
	}
}

// setupInstanceMetadata configures all metadata-related settings for the instance.
// sourcePoolName is the pool whose template was used as the bootstrap source;
// it is stripped from GKE built-in labels so the provisioned node is not
// associated with it.
// ssdCount is the pre-resolved local-SSD count from resolveLocalSSDCount; the
// caller threads it down so we don't re-fetch the MachineType here (the second
// cache lookup would race against UpdateInstanceTypes between the call sites).
func (p *DefaultProvider) setupInstanceMetadata(ctx context.Context, instanceMetadata *compute.Metadata, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, nodeClaim *karpv1.NodeClaim, sourcePoolName string, capacityType string, isGPUInstance bool, ssdCount int) error {
	if err := metadata.RemoveGKEBuiltinLabels(instanceMetadata, sourcePoolName); err != nil {
		return fmt.Errorf("failed to remove GKE builtin labels from metadata: %w", err)
	}

	if err := metadata.SetMaxPodsPerNode(instanceMetadata, nodeClass); err != nil {
		return fmt.Errorf("failed to set max pods per node in metadata: %w", err)
	}

	if err := metadata.RenderKubeletConfigMetadata(instanceMetadata, instanceType, capacityType); err != nil {
		return fmt.Errorf("failed to render kubelet config metadata: %w", err)
	}

	if err := metadata.PatchUnregisteredTaints(instanceMetadata); err != nil {
		return fmt.Errorf("failed to append unregistered taint to kube-env: %w", err)
	}

	if err := p.patchKubeEnv(ctx, instanceMetadata, nodeClass, instanceType); err != nil {
		return err
	}

	if capacityType == karpv1.CapacityTypeSpot {
		if err := metadata.SetProvisioningModel(instanceMetadata, capacityType); err != nil {
			return fmt.Errorf("failed to set provisioning model in metadata: %w", err)
		}
	}

	metadata.AppendNodeClaimLabel(nodeClaim, nodeClass, instanceMetadata)
	metadata.AppendRegisteredLabel(instanceMetadata)
	metadata.AppendSecondaryBootDisks(p.projectID, nodeClass, instanceMetadata)

	if err := patchLocalSSDMetadata(instanceMetadata, nodeClass, ssdCount); err != nil {
		return err
	}

	metadata.ApplyCustomMetadata(instanceMetadata, nodeClass.Spec.Metadata)

	// GPU labels are injected last so that spec.gpuDriverVersion is always authoritative,
	// overriding both the base template value and any spec.metadata entry.
	if err := setupGPUMetadata(instanceMetadata, nodeClass, instanceType, isGPUInstance); err != nil {
		return fmt.Errorf("failed to inject GPU metadata labels: %w", err)
	}

	return nil
}

// patchLocalSSDMetadata applies the local-SSD kube-env / kube-labels patch when
// ssdCount > 0. No-op for count==0 so the caller doesn't have to guard.
func patchLocalSSDMetadata(instanceMetadata *compute.Metadata, nodeClass *v1alpha1.GCENodeClass, ssdCount int) error {
	if ssdCount <= 0 {
		return nil
	}
	patched, err := metadata.PatchLocalSSDMetadata(
		instanceMetadata.Items,
		nodeClass.Spec.LocalSsdMode,
		ssdCount,
	)
	if err != nil {
		return fmt.Errorf("failed to patch local SSD metadata: %w", err)
	}
	instanceMetadata.Items = patched
	return nil
}

// CreateError reasons emitted by resolveLocalSSDCount. Surface as
// NodeClaim.Status.Conditions[Launched].Reason via *cloudprovider.CreateError.
const (
	reasonUnsupportedLocalSSDCountOperator = "UnsupportedLocalSSDCountOperator"
	reasonMultiValuedLocalSSDCount         = "MultiValuedLocalSSDCount"
	reasonInvalidLocalSSDCountValue        = "InvalidLocalSSDCountValue"
)

// resolveLocalSSDCount returns the effective local-SSD count to advertise to
// the GKE bootstrapper via kube-env. Precedence:
//
//  1. machineType.BundledLocalSsds.PartitionCount — bundled SKUs (z3, c4d-lssd,
//     c4-lssd, c4a-lssd, etc.). Bundled count is authoritative; intersection
//     narrowing on the InstanceType requirement should have either matched any
//     NodeClaim count requirement or filtered the SKU out, so we don't
//     reconcile against the requirement here.
//  2. NodeClaim count requirement (key v1alpha1.LabelInstanceLocalSsdCount).
//     Operator != In returns a *cloudprovider.CreateError (deliberate scope
//     cut — supporting Gt/Lt/NotIn requires per-machine-type allowed-count
//     metadata we don't keep). In with len > 1 is also a CreateError; the
//     resolver picks one physical count and can't represent "either N or M."
//  3. Legacy disks[].category=local-ssd entries — kept as a transitional
//     fallback until disks[] is fully retired.
//
// Returns a plain (retryable) error when the name suffix says the SKU bundles
// local SSDs but mt is nil (cache miss). The caller wraps that in
// &retryableError{} so the Create loop falls through to the next candidate
// rather than booting a node whose physical SSDs would never be formatted.
func resolveLocalSSDCount(
	reqs scheduling.Requirements,
	nodeClass *v1alpha1.GCENodeClass,
	instanceTypeName string,
	mt *computepb.MachineType,
) (int, error) {
	if mt != nil {
		if bls := mt.GetBundledLocalSsds(); bls != nil && bls.PartitionCount != nil && *bls.PartitionCount > 0 {
			return int(*bls.PartitionCount), nil
		}
	} else if hasBundledLocalSSDs(instanceTypeName, nil) {
		return 0, fmt.Errorf("machine type %s bundles local SSDs but is not in the instance-type cache; refusing to create without an authoritative SSD count", instanceTypeName)
	}

	// scheduling.Requirements.Get synthesizes a NodeSelectorOpExists requirement
	// for an absent key, so we must check Has first; an absent count requirement
	// is not an error, it falls through to legacy disks[] / zero.
	if reqs.Has(v1alpha1.LabelInstanceLocalSsdCount) {
		count, ok, err := countFromSSDRequirement(reqs.Get(v1alpha1.LabelInstanceLocalSsdCount))
		if err != nil {
			return 0, err
		}
		if ok {
			return count, nil
		}
	}

	legacy := 0
	for _, d := range nodeClass.Spec.Disks {
		if d.Category == "local-ssd" {
			legacy++
		}
	}
	return legacy, nil
}

// countFromSSDRequirement parses a karpenter.k8s.gcp/instance-local-ssd-count
// requirement. Returns (count, true, nil) on a single valid In value;
// (0, false, nil) when the requirement is effectively absent (DoesNotExist or
// In:[]) so the caller falls through to legacy disks[]; and a
// *cloudprovider.CreateError when the operator is unsupported, the value list
// has more than one entry, or the single value isn't a non-negative integer.
func countFromSSDRequirement(req *scheduling.Requirement) (int, bool, error) {
	op := req.Operator()
	if op == corev1.NodeSelectorOpDoesNotExist {
		return 0, false, nil
	}
	if op != corev1.NodeSelectorOpIn {
		return 0, false, cloudprovider.NewCreateError(
			fmt.Errorf("%s requirement uses operator %s; only In is supported", v1alpha1.LabelInstanceLocalSsdCount, op),
			reasonUnsupportedLocalSSDCountOperator,
			fmt.Sprintf("requirement %s uses operator %s; only In is supported", v1alpha1.LabelInstanceLocalSsdCount, op),
		)
	}
	values := req.Values()
	switch len(values) {
	case 0:
		return 0, false, nil
	case 1:
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 0 {
			return 0, false, cloudprovider.NewCreateError(
				fmt.Errorf("%s requirement value %q is not a non-negative integer", v1alpha1.LabelInstanceLocalSsdCount, values[0]),
				reasonInvalidLocalSSDCountValue,
				fmt.Sprintf("requirement %s value %q is not a non-negative integer", v1alpha1.LabelInstanceLocalSsdCount, values[0]),
			)
		}
		return n, true, nil
	default:
		return 0, false, cloudprovider.NewCreateError(
			fmt.Errorf("%s requirement resolved to %d values (%v); resolver requires a single In value", v1alpha1.LabelInstanceLocalSsdCount, len(values), values),
			reasonMultiValuedLocalSSDCount,
			fmt.Sprintf("requirement %s resolved to %d values; expected exactly one", v1alpha1.LabelInstanceLocalSsdCount, len(values)),
		)
	}
}

// warnLegacyLocalSSDDisks logs a deprecation warning for each
// `spec.disks[].category == "local-ssd"` entry on the NodeClass. Emitted per
// provisioning attempt (not once at apply time) so the warning lands next to
// the provisioning log line in the controller output.
func warnLegacyLocalSSDDisks(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) {
	for i, d := range nodeClass.Spec.Disks {
		if d.Category != "local-ssd" {
			continue
		}
		fields := []any{"nodeClass", nodeClass.Name, "diskIndex", i}
		if d.SizeGiB > 0 {
			fields = append(fields, "sizeGiB", d.SizeGiB, "sizeGiBIgnored", true)
		}
		log.FromContext(ctx).Info(
			"spec.disks[].category=local-ssd is deprecated; pin the count on the pod or NodePool via the karpenter.k8s.gcp/instance-local-ssd-count label instead",
			fields...,
		)
	}
}

// instanceTypeHasGPU reports whether the instance type has a built-in GPU requirement.
// It uses direct map access because Requirements.Get() for a missing key returns
// NodeSelectorOpExists with Len()=MaxInt64, which would incorrectly classify all
// instance types as GPU instances.
func instanceTypeHasGPU(instanceType *cloudprovider.InstanceType) bool {
	req := instanceType.Requirements[v1alpha1.LabelInstanceGPUCount]
	return req != nil && req.Operator() == corev1.NodeSelectorOpIn && req.Len() > 0
}

// patchKubeEnv applies all kube-env patches: instance type (arch/family), OS type, and arch binary URLs.
func (p *DefaultProvider) patchKubeEnv(ctx context.Context, instanceMetadata *compute.Metadata, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType) error {
	if err := metadata.PatchKubeEnvForInstanceType(instanceMetadata, instanceType); err != nil {
		return fmt.Errorf("failed to patch kube-env for instance type: %w", err)
	}
	if err := metadata.PatchKubeEnvForOSType(instanceMetadata, nodeClass.ImageFamily()); err != nil {
		return fmt.Errorf("failed to patch kube-env for OS type: %w", err)
	}
	if err := p.patchKubeEnvForArch(ctx, instanceMetadata, instanceType); err != nil {
		return err
	}
	return nil
}

// patchKubeEnvForArch patches SERVER_BINARY_TAR_URL/HASH in the kube-env when the
// target arch differs from the source pool's arch. The GKE release version is read
// from the Kubernetes API server (Group 2 via GKE API) rather than parsed from the
// pool template's kube-env URL, making the patch independent of the URL format.
func (p *DefaultProvider) patchKubeEnvForArch(ctx context.Context, instanceMetadata *compute.Metadata, instanceType *cloudprovider.InstanceType) error {
	arch := instanceType.Requirements.Get(corev1.LabelArchStable).Any()
	if arch == "" {
		arch = imagefamily.OSArchAMD64Requirement
	}
	gkeVersion := p.resolveGKEVersion(ctx)
	if err := metadata.PatchKubeEnvForArch(ctx, instanceMetadata, arch, gkeVersion, http.DefaultClient); err != nil {
		return fmt.Errorf("failed to patch kube-env for arch %s: %w", arch, err)
	}
	return nil
}

// resolveGKEVersion returns the GKE release version string from the version provider,
// or empty string on error (caller falls back to URL-based version detection).
func (p *DefaultProvider) resolveGKEVersion(ctx context.Context) string {
	if p.versionProvider == nil {
		return ""
	}
	v, err := p.versionProvider.Get(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to get GKE version for arch patching; falling back to URL parsing")
		return ""
	}
	return v
}

// setupGPUMetadata handles all GPU-specific metadata injection.
// AutoGPUTaint applies to any GPU node (including attached GPU instances).
// The accelerator label is set only for built-in GPU families where the
// accelerator type is known from instance type requirements.
// The driver-version label is set for all GPU nodes (built-in and attached).
func setupGPUMetadata(instanceMetadata *compute.Metadata, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, isGPU bool) error {
	if !isGPU {
		return nil
	}
	if nodeClass.Spec.AutoGPUTaint {
		if err := metadata.AppendGPUTaint(instanceMetadata); err != nil {
			return fmt.Errorf("failed to append GPU taint to kube-env: %w", err)
		}
	}
	// Use direct map access: Requirements.Get() for a missing key returns NodeSelectorOpExists
	// with Len()=MaxInt64 and Any()=random integer, which would inject a bogus accelerator label.
	var gpuName string
	if req := instanceType.Requirements[v1alpha1.LabelGKEAccelerator]; req != nil && req.Operator() == corev1.NodeSelectorOpIn && req.Len() > 0 {
		gpuName = req.Any()
	}
	if gpuName != "" {
		if err := metadata.SetGPUAcceleratorLabel(instanceMetadata, gpuName); err != nil {
			return fmt.Errorf("gke-accelerator: %w", err)
		}
	}
	version := nodeClass.Spec.GPUDriverVersion
	if version == "" {
		version = "default"
	}
	if err := metadata.SetGPUDriverVersionLabel(instanceMetadata, version); err != nil {
		return fmt.Errorf("gke-gpu-driver-version: %w", err)
	}
	return nil
}

// setupServiceAccounts configures service accounts for the instance.
// Returns an error if no service account can be resolved, which means the project has
// disabled the Compute Engine default SA and neither spec.serviceAccount nor
// DEFAULT_NODEPOOL_SERVICE_ACCOUNT is set — nodes would fail to authenticate without an SA.
func (p *DefaultProvider) setupServiceAccounts(nodeClass *v1alpha1.GCENodeClass) ([]*compute.ServiceAccount, error) {
	serviceAccount := p.resolveServiceAccount(nodeClass)
	if serviceAccount == "" {
		return nil, fmt.Errorf("no service account available: spec.serviceAccount and DEFAULT_NODEPOOL_SERVICE_ACCOUNT are unset, and the Compute Engine default SA is unavailable for this project")
	}

	return []*compute.ServiceAccount{
		{
			Email: serviceAccount,
			Scopes: []string{
				// cloud-platform scope provides full access to all Google Cloud Platform APIs
				// Note: This is a broad scope
				// However, since GCENodeClass doesn't support custom OAuth scopes configuration,
				// we use this as a compromise to ensure the instance has necessary permissions
				// for basic operations like VM management, storage access, and container registry access.
				// TODO: When NodeClass API supports custom OAuth scopes, replace with more restrictive scopes
				"https://www.googleapis.com/auth/cloud-platform",
			},
		},
	}, nil
}

// setupScheduling returns scheduling config derived from capacity type alone.
// Spot-specific fields (provisioning model, preemptibility) are set later by
// configureInstanceCapacityProvision; this only wires the termination action so
// GCE honors DELETE rather than the default STOP on preemption.
func setupScheduling(capacityType string) *compute.Scheduling {
	sched := &compute.Scheduling{}
	if capacityType == karpv1.CapacityTypeSpot {
		sched.InstanceTerminationAction = instanceTerminationActionDelete
	}
	return sched
}

// initializeInstanceLabels initializes the instance labels map
func (p *DefaultProvider) initializeInstanceLabels(nodeClass *v1alpha1.GCENodeClass) map[string]string {
	labels := make(map[string]string)
	for k, v := range nodeClass.Spec.Labels {
		labels[k] = v
	}
	return labels
}

// configureInstanceCapacityProvision configures capacity provision settings for the instance
func (p *DefaultProvider) configureInstanceCapacityProvision(instance *compute.Instance, capacityType string) {
	if instance.Scheduling == nil {
		instance.Scheduling = &compute.Scheduling{}
	}

	if capacityType == karpv1.CapacityTypeSpot {
		instance.Scheduling.ProvisioningModel = "SPOT"
		instance.Scheduling.Preemptible = true
		instance.Scheduling.AutomaticRestart = ptr.To(false)
	}
}

// z3HighSsdGiBThreshold is the bundled-local-SSD capacity above which a z3
// non-metal SKU must use TERMINATE per GCE rules. Per GCP docs
// (https://cloud.google.com/compute/docs/instances/setting-vm-host-options),
// TERMINATE is the only supported maintenance policy for z3 instances with
// more than 18 TiB of attached Titanium SSD. Below the threshold, z3
// non-metal requires MIGRATE.
const z3HighSsdGiBThreshold = 18 * 1024

// onHostMaintenancePolicy returns the GCE Scheduling.OnHostMaintenance value
// required by the (instanceType, capacityType, machineType) tuple, or "" to
// leave the GCE default (MIGRATE for non-spot non-metal).
// Precedence: spot → GPU → z3-non-metal >18 TiB → z3-non-metal → bare-metal → h4d → default.
//
// Spot + z3-non-metal-lssd: spot requires TERMINATE, z3 non-metal lssd ≤18 TiB
// requires MIGRATE; GCE rejects either combination. We pick TERMINATE so
// the failure surfaces as the user's explicit spot request rather than a
// silent flip to on-demand.
//
// z3 non-metal with >18 TiB bundled SSD: GCE rejects MIGRATE for these SKUs
// (e.g. z3-highmem-88-highlssd, z3-highmem-176-standardlssd, both at 12 ×
// 3000 GiB = ~35 TiB). The mt argument is the cache entry; when nil we fall
// back to MIGRATE — the cache is normally populated for any SKU we
// schedule, and a future high-SSD z3 SKU hitting an empty cache will fail
// loud at GCE creation time rather than silently misbehave.
//
// Bare-metal (`*-metal`): physically cannot live-migrate (no VM-level state
// to copy out-of-band). GCE's current default is TERMINATE, but we set it
// explicitly so the branch contract is self-documenting rather than
// relying on GCE's default.
//
// H4D (`h4d-*`): CPU-only HPC family that never supports live migration
// (RDMA fabric requirements per AI Hypercomputer docs). TERMINATE is the
// only supported policy. Set explicitly for the same reason as bare-metal.
func onHostMaintenancePolicy(instanceType *cloudprovider.InstanceType, capacityType string, mt *computepb.MachineType) string {
	if capacityType == karpv1.CapacityTypeSpot {
		return "TERMINATE"
	}
	if instanceType.Requirements.Get(v1alpha1.LabelInstanceGPUCount).Len() > 0 {
		return "TERMINATE"
	}
	if strings.HasPrefix(instanceType.Name, "z3-") && !strings.HasSuffix(instanceType.Name, "-metal") {
		if mt != nil {
			if bls := mt.GetBundledLocalSsds(); bls != nil && bls.PartitionCount != nil {
				if localssd.TotalGiB(instanceType.Name, int(*bls.PartitionCount)) > z3HighSsdGiBThreshold {
					return "TERMINATE"
				}
			}
		}
		return "MIGRATE"
	}
	if strings.HasSuffix(instanceType.Name, "-metal") {
		return "TERMINATE"
	}
	if strings.HasPrefix(instanceType.Name, "h4d-") {
		return "TERMINATE"
	}
	return ""
}

// setupInstanceLabels writes all GCE labels for a new instance. Cluster-identity labels
// (goog-k8s-cluster-name and goog-k8s-cluster-location) are always written last so that
// user-supplied NodePool requirement labels with the same key cannot overwrite them.
func (p *DefaultProvider) setupInstanceLabels(instance *compute.Instance, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType) {
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelNodePoolKey)] = nodeClaim.Labels[karpv1.NodePoolLabelKey]
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey)] = nodeClass.Name

	for key, req := range instanceType.Requirements {
		if req.Operator() == corev1.NodeSelectorOpIn && req.Len() == 1 {
			// GCP label keys only allow [a-z0-9_-]; Kubernetes label keys may contain '/' and '.'.
			instance.Labels[utils.SanitizeGCELabelValue(key)] = utils.SanitizeGCELabelValue(req.Any())
		}
	}

	// Stamp both cluster-identity labels last so NodePool labels cannot overwrite them.
	// LabelClusterNameKey is used by the syncInstances API filter; LabelClusterLocationKey
	// is checked by belongsToCluster to distinguish same-named clusters in different locations.
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelClusterNameKey)] = p.clusterName
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)] = p.clusterLocation
}

// belongsToCluster reports whether inst should be tracked in this controller's instance
// cache. An instance with goog-k8s-cluster-location present must match this cluster's
// location. An instance without the label (created by an older Karpenter version) is
// treated as ours for backward compatibility — the GC controller separately skips
// label-less instances to prevent cross-cluster deletion. Cluster-name is enforced by the
// GCE API label filter in syncInstances.
//
// TODO: once all instances carry goog-k8s-cluster-location (i.e. after all clusters have
// been upgraded past this release and all pre-existing nodes have been rotated), replace
// the !ok fallback with strict matching (ok && loc == p.clusterLocation) and add the
// label as a server-side filter in syncInstances. Until then the fallback must stay to
// avoid making pre-label instances invisible to Karpenter.
func (p *DefaultProvider) belongsToCluster(inst *Instance) bool {
	loc, ok := inst.Labels[utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)]
	return !ok || loc == p.clusterLocation
}

// buildInstanceTags emits the generic gke-<clusterName>-node tag and, when
// clusterID is at least 8 characters, the cluster-id-bearing
// gke-<clusterName>-<clusterID[:8]>-node tag (the target of GKE's
// auto-created cluster firewall rules). NodeClass network tags are appended
// last. clusterID is normally the 64-char cluster.Id from the Container API;
// the cluster-id tag is omitted if the API returns a short or empty value.
func buildInstanceTags(clusterName, clusterID string, networkTags []v1alpha1.NetworkTag) *compute.Tags {
	items := make([]string, 0, 2+len(networkTags))
	items = append(items, fmt.Sprintf("gke-%s-node", clusterName))
	if len(clusterID) >= 8 {
		items = append(items, fmt.Sprintf("gke-%s-%s-node", clusterName, clusterID[:8]))
	}
	for _, t := range networkTags {
		items = append(items, string(t))
	}
	return &compute.Tags{Items: lo.Uniq(items)}
}

func (p *DefaultProvider) Get(ctx context.Context, providerID string) (*Instance, error) {
	_, _, instanceName, err := parseGCEProviderID(providerID)
	if err != nil {
		return nil, fmt.Errorf("parsing provider ID: %w", err)
	}

	if instance, ok := p.instanceCache.Get(instanceName); ok {
		return instance.(*Instance), nil
	}

	if err := p.syncInstances(ctx); err != nil {
		log.FromContext(ctx).Error(err, "syncing instances")
		return nil, err
	}

	currentInstance, ok := p.instanceCache.Get(instanceName)
	if ok {
		return currentInstance.(*Instance), nil
	}

	return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance(%s) not found ", providerID))
}

func parseCreationTime(ts string) time.Time {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Now() // fallback
	}
	return parsed
}

func getBootImageID(inst *compute.Instance) string {
	if len(inst.Disks) > 0 && inst.Disks[0].InitializeParams != nil {
		return inst.Disks[0].InitializeParams.SourceImage
	}
	return ""
}

func resolveCapacityType(sched *compute.Scheduling) string {
	if sched != nil && sched.ProvisioningModel == "SPOT" {
		return karpv1.CapacityTypeSpot
	}
	return karpv1.CapacityTypeOnDemand
}

func (p *DefaultProvider) List(ctx context.Context) ([]*Instance, error) {
	if err := p.syncInstances(ctx); err != nil {
		log.FromContext(ctx).Error(err, "syncing instances failed")
		return nil, err
	}

	var instances []*Instance
	for _, instance := range p.instanceCache.Items() {
		instances = append(instances, instance.Object.(*Instance))
	}

	return instances, nil
}

func (p *DefaultProvider) getZonesInRegion(ctx context.Context) ([]string, error) {
	region, err := p.computeService.Regions.Get(p.projectID, p.region).Context(ctx).Do()
	if err != nil {
		return nil, err
	}

	var zones []string
	for _, zoneURL := range region.Zones {
		parts := strings.Split(zoneURL, "/")
		zones = append(zones, parts[len(parts)-1])
	}
	return zones, nil
}

func (p *DefaultProvider) Delete(ctx context.Context, providerID string) error {
	project, zone, instanceName, err := parseGCEProviderID(providerID)
	if err != nil {
		return fmt.Errorf("parsing provider ID: %w", err)
	}

	log.FromContext(ctx).Info("Deleting instance", "project", project, "zone", zone, "instance", instanceName)

	// Check current state before issuing a delete. This prevents sending multiple overlapping
	// delete operations to GCE (which cause 403 rateLimitExceeded on repeated reconciles).
	inst, err := p.computeService.Instances.Get(project, zone, instanceName).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			log.FromContext(ctx).Info("Instance already deleted", "instance", instanceName)
			return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance not found: %w", err))
		}
		return fmt.Errorf("getting instance state before delete: %w", err)
	}

	switch inst.Status {
	case InstanceStatusTerminated:
		log.FromContext(ctx).Info("Instance already terminated", "instance", instanceName)
		return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance %s is already %s", instanceName, inst.Status))
	case InstanceStatusStopping:
		// Delete already in flight; caller re-queues and rechecks.
		log.FromContext(ctx).Info("Instance delete already in progress", "instance", instanceName)
		return nil
	}

	op, err := p.computeService.Instances.Delete(project, zone, instanceName).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			log.FromContext(ctx).Info("Instance disappeared before delete call", "instance", instanceName)
			return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance not found: %w", err))
		}
		return fmt.Errorf("deleting instance: %w", err)
	}

	log.FromContext(ctx).Info("Delete operation started", "operation", op.Name)
	return nil
}

func (p *DefaultProvider) CreateTags(ctx context.Context, providerID string, tags map[string]string) error {
	projectID, zone, instanceName, err := parseGCEProviderID(providerID)
	if err != nil {
		return fmt.Errorf("failed to parse provider ID: %w", err)
	}

	instance, err := p.computeService.Instances.Get(projectID, zone, instanceName).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			log.FromContext(ctx).Info("Instance not found", "instance", instanceName)
			return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance not found: %w", err))
		}
		return fmt.Errorf("getting instance: %w", err)
	}

	newLabels := instance.Labels
	if newLabels == nil {
		newLabels = make(map[string]string)
	}
	for k, v := range tags {
		newLabels[k] = v
	}

	req := &compute.InstancesSetLabelsRequest{
		Labels:           newLabels,
		LabelFingerprint: instance.LabelFingerprint,
	}

	_, err = p.computeService.Instances.SetLabels(projectID, zone, instanceName, req).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("setting labels: %w", err)
	}

	// List and sync instances to update the cache
	_, err = p.List(ctx)
	if err != nil {
		return err
	}

	return err
}

func parseGCEProviderID(providerID string) (project, zone, instance string, err error) {
	const prefix = "gce://"
	if !strings.HasPrefix(providerID, prefix) {
		err = fmt.Errorf("unexpected providerID format: %s", providerID)
		return
	}

	parts := strings.Split(strings.TrimPrefix(providerID, prefix), "/")
	if len(parts) != 3 {
		err = fmt.Errorf("invalid GCE providerID, expected format gce://project/zone/instance")
		return
	}

	project, zone, instance = parts[0], parts[1], parts[2]
	return
}

func isInstanceNotFoundError(err error) bool {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == 404
	}
	return false
}

func (p *DefaultProvider) syncInstances(ctx context.Context) error {
	var instances []*Instance
	filter := fmt.Sprintf(
		"(labels.%s:* AND labels.%s:* AND labels.%s:%s)",
		utils.SanitizeGCELabelValue(utils.LabelNodePoolKey),
		utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey),
		utils.SanitizeGCELabelValue(utils.LabelClusterNameKey),
		p.clusterName,
	)

	zones, err := p.getZonesInRegion(ctx)
	if err != nil {
		return fmt.Errorf("listing zones, %w", err)
	}

	for _, zone := range zones {
		req := p.computeService.Instances.List(p.projectID, zone).Filter(filter).Context(ctx)

		err := req.Pages(ctx, func(page *compute.InstanceList) error {
			if len(page.Items) == 0 {
				return nil // empty page, continue
			}

			for _, inst := range page.Items {
				instance := &Instance{
					InstanceID:   inst.Name,
					Name:         inst.Name,
					Type:         inst.MachineType[strings.LastIndex(inst.MachineType, "/")+1:], // just for matching "e2-standard-2"
					Location:     zone,
					ProjectID:    p.projectID,
					ImageID:      getBootImageID(inst),
					CreationTime: parseCreationTime(inst.CreationTimestamp),
					CapacityType: resolveCapacityType(inst.Scheduling),
					Labels:       inst.Labels,
					Tags:         inst.Labels,
					Status:       inst.Status,
				}
				instances = append(instances, instance)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("listing instances in zone %s: %w", zone, err)
		}
	}

	for _, inst := range instances {
		if p.belongsToCluster(inst) {
			p.instanceCache.Set(inst.InstanceID, inst, cache.DefaultExpiration)
		}
	}
	return nil
}

// resolveServiceAccount returns the service account email for the instance.
// Priority: spec.serviceAccount → DEFAULT_NODEPOOL_SERVICE_ACCOUNT → Compute Engine default SA.
func (p *DefaultProvider) resolveServiceAccount(nodeClass *v1alpha1.GCENodeClass) string {
	if nodeClass.Spec.ServiceAccount != "" {
		return nodeClass.Spec.ServiceAccount
	}
	if p.defaultServiceAccount != "" {
		return p.defaultServiceAccount
	}
	return p.computeDefaultSA
}
