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

package localssd

import "strings"

// DefaultPartitionGiB is the standard NVMe local SSD partition size for most GCP machine families.
const DefaultPartitionGiB int64 = 375

// configurableFamilyPrefixes lists machine-family prefixes whose SKUs accept a
// caller-supplied local-SSD count (i.e., not bundled, not no-SSD-only).
// Verified 2026-05 against `gcloud compute machine-types describe`; n3 is
// deliberately absent because no n3 family currently exists in GCP.
var configurableFamilyPrefixes = []string{"n1-", "n2-", "n2d-", "c2-", "c2d-"}

// FamilySupportsConfigurableLocalSSDs reports whether the machine-type name's
// family accepts a caller-specified local-SSD count. The scheduler uses this
// to decide whether to emit per-count InstanceType variants (configurable)
// or a single In:["0"] / In:["<bundled>"] requirement.
//
// Bundled-SSD SKUs (e.g. c4d-...-lssd, z3-..., a3-...) are NOT in this list;
// they get their count from MachineType.BundledLocalSsds.PartitionCount.
func FamilySupportsConfigurableLocalSSDs(machineName string) bool {
	for _, p := range configurableFamilyPrefixes {
		if strings.HasPrefix(machineName, p) {
			return true
		}
	}
	return false
}

// AllowedLocalSSDCounts returns the SSD counts GCE accepts at instance-create
// time for an unbundled (count-configurable) machine type, derived from the
// per-SKU tables in GCE's machine-family docs. The set excludes 0; callers
// emit the zero variant separately.
//
// Only n1, n2, n2d, c2, and c2d have count-configurable local SSDs. All newer
// families ship bundled SSDs (machine names ending -lssd / -standardlssd /
// -highlssd, plus a3/a4) where the count is fixed by the SKU. New families
// are not expected to revive the configurable pattern, so this table is
// effectively static.
//
// Returns nil for machine types not in a configurable family (gate on
// FamilySupportsConfigurableLocalSSDs first) or below a family's minimum SKU
// vCPU count.
//
// Verified 2026-05 against:
//   - https://cloud.google.com/compute/docs/general-purpose-machines (n1, n2, n2d)
//   - https://cloud.google.com/compute/docs/compute-optimized-machines (c2, c2d)
func AllowedLocalSSDCounts(machineName string, vCPUs int32) []int {
	family, ok := configurableLocalSSDFamilies[familyPrefix(machineName)]
	if !ok {
		return nil
	}
	if family.fixed != nil {
		return family.fixed
	}
	for _, b := range family.brackets {
		if vCPUs >= b.minVCPUs {
			return b.counts
		}
	}
	return nil
}

// vcpuBracket is one entry of a configurable family's per-vCPU allowed-count
// table. Brackets within a family are ordered descending by minVCPUs; the
// first match wins.
type vcpuBracket struct {
	minVCPUs int32
	counts   []int
}

// configurableFamily describes a count-configurable SSD family. Set fixed for
// families that accept the same set across all vCPU counts (n1); otherwise set
// brackets for the per-vCPU rule.
type configurableFamily struct {
	fixed    []int
	brackets []vcpuBracket
}

// configurableLocalSSDFamilies pins the per-family GCE allowed-count tables.
// Bracket boundaries match GCE's published per-vCPU table verbatim (e.g. n2's
// "22–40 bracket"), not the vCPU counts of currently-predefined SKUs; the two
// coincide today but pinning to the doc rule means we stay correct if GCE
// ever adds an intermediate SKU.
var configurableLocalSSDFamilies = map[string]configurableFamily{
	// n1's "1 to 8, 16, or 24" includes odd counts. All n1 SKUs including
	// n1-standard-1 accept the full set; n1 is legacy and unlikely to gain
	// new SKUs, so a static set is safe.
	"n1": {fixed: []int{1, 2, 3, 4, 5, 6, 7, 8, 16, 24}},
	"n2": {brackets: []vcpuBracket{
		{minVCPUs: 82, counts: []int{16, 24}},
		{minVCPUs: 42, counts: []int{8, 16, 24}},
		{minVCPUs: 22, counts: []int{4, 8, 16, 24}},
		{minVCPUs: 12, counts: []int{2, 4, 8, 16, 24}},
		{minVCPUs: 2, counts: []int{1, 2, 4, 8, 16, 24}},
	}},
	"n2d": {brackets: []vcpuBracket{
		{minVCPUs: 96, counts: []int{8, 16, 24}},
		{minVCPUs: 64, counts: []int{4, 8, 16, 24}},
		{minVCPUs: 32, counts: []int{2, 4, 8, 16, 24}},
		{minVCPUs: 2, counts: []int{1, 2, 4, 8, 16, 24}},
	}},
	"c2": {brackets: []vcpuBracket{
		{minVCPUs: 60, counts: []int{8}},
		{minVCPUs: 30, counts: []int{4, 8}},
		{minVCPUs: 16, counts: []int{2, 4, 8}},
		{minVCPUs: 4, counts: []int{1, 2, 4, 8}},
	}},
	"c2d": {brackets: []vcpuBracket{
		{minVCPUs: 112, counts: []int{8}},
		{minVCPUs: 56, counts: []int{4, 8}},
		{minVCPUs: 32, counts: []int{2, 4, 8}},
		{minVCPUs: 2, counts: []int{1, 2, 4, 8}},
	}},
}

func familyPrefix(machineName string) string {
	if i := strings.IndexByte(machineName, '-'); i > 0 {
		return machineName[:i]
	}
	return machineName
}

type entry struct {
	totalGiB   int64 // total SSD capacity; 0 = compute from partitions
	perPartGiB int64 // per-partition GiB; 0 = use DefaultPartitionGiB
}

// table maps machine families (no "-") and specific machine types (contains "-") to their
// local SSD sizing. Family entries override per-partition GiB; machine entries override the total
// for machines where the Compute API returns a wrong PartitionCount.
//
// Source: https://github.com/Cyclenerd/google-cloud-pricing-cost-calculator/blob/master/build/gcp.yml
// Cross-referenced with: https://cloud.google.com/compute/docs/disks/local-ssd
var table = map[string]entry{
	// z3 uses 3 TiB NVMe per partition; all other families use 375 GiB
	"z3": {perPartGiB: 3000},

	// Bare-metal variants use 3000 GiB per partition (not 375 GiB)
	"c4-highmem-288-lssd-metal":     {totalGiB: 18000}, // 6 × 3000 GiB
	"c4-standard-288-lssd-metal":    {totalGiB: 18000}, // 6 × 3000 GiB
	"z3-highmem-192-highlssd-metal": {totalGiB: 72000}, // 12 × 6000 GiB
}

// TotalGiB returns total local SSD capacity in GiB for the given machine type.
// Machine-level total overrides take priority (for machines where the API reports a wrong
// PartitionCount); otherwise falls back to partitionCount × per-family partition size.
func TotalGiB(machineName string, partitionCount int) int64 {
	if e, ok := table[machineName]; ok && e.totalGiB > 0 {
		return e.totalGiB
	}
	if partitionCount <= 0 {
		return 0
	}
	return int64(partitionCount) * partitionSizeGiB(machineName)
}

// partitionSizeGiB returns the GiB capacity of a single local SSD partition for the given
// machine type, using a family-level override from table or DefaultPartitionGiB.
func partitionSizeGiB(machineName string) int64 {
	family := machineName
	if i := strings.IndexByte(machineName, '-'); i > 0 {
		family = machineName[:i]
	}
	if e, ok := table[family]; ok && e.perPartGiB > 0 {
		return e.perPartGiB
	}
	return DefaultPartitionGiB
}
