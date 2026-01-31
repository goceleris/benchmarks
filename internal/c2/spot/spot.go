// Package spot provides AWS spot instance management for the C2 server.
// It handles spot pricing, AZ selection, and instance lifecycle.
package spot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"

	"github.com/goceleris/benchmarks/internal/c2/awsutil"
)

// SpotQuotaCode is the AWS Service Quotas code for "All Standard Spot Instance Requests"
const SpotQuotaCode = "L-34B43A08"

// instanceVCPUs maps instance types to their vCPU counts.
// Used for quota calculations.
var instanceVCPUs = map[string]int{
	// ARM64 - C6g family
	"c6g.medium":  1,
	"c6g.large":   2,
	"c6g.xlarge":  4,
	"c6g.2xlarge": 8,
	"c6g.4xlarge": 16,
	"c6g.8xlarge": 32,
	"c6g.metal":   64,
	// ARM64 - C7g family
	"c7g.medium":  1,
	"c7g.large":   2,
	"c7g.xlarge":  4,
	"c7g.2xlarge": 8,
	"c7g.4xlarge": 16,
	"c7g.metal":   64,
	// ARM64 - T4g family
	"t4g.micro":  2,
	"t4g.small":  2,
	"t4g.medium": 2,
	// ARM64 - M7g family
	"m7g.metal": 64,

	// x86 - C5 family
	"c5.large":   2,
	"c5.xlarge":  4,
	"c5.2xlarge": 8,
	"c5.9xlarge": 36,
	"c5.metal":   96,
	// x86 - C5a family
	"c5a.large":   2,
	"c5a.xlarge":  4,
	"c5a.2xlarge": 8,
	// x86 - C5d family
	"c5d.9xlarge": 36,
	"c5d.metal":   96,
	// x86 - C5n family
	"c5n.9xlarge": 36,
	"c5n.metal":   72,
	// x86 - C6i family
	"c6i.large":   2,
	"c6i.xlarge":  4,
	"c6i.2xlarge": 8,
	// x86 - T2/T3 family
	"t2.small":  1,
	"t3.small":  2,
	"t3a.small": 2,
}

// PriceCacheTTL is how long cached on-demand prices are valid.
const PriceCacheTTL = 1 * time.Hour

// cachedPrice holds a cached on-demand price with expiration.
type cachedPrice struct {
	price     float64
	expiresAt time.Time
}

// Client wraps AWS EC2 client with spot-specific operations.
type Client struct {
	ec2     *ec2.Client
	pricing *pricing.Client
	region  string

	// Price cache for on-demand prices (thread-safe)
	priceMu    sync.RWMutex
	priceCache map[string]*cachedPrice // key: "region:instanceType"
}

// SupportedRegions lists the AWS regions we support for benchmarks.
// These regions were selected for:
// - Lower spot prices for compute-optimized instances
// - Good availability of metal instances (c5.metal, c6g.metal)
// - Geographic distribution for global coverage
var SupportedRegions = []string{
	"us-east-1",      // N. Virginia - Largest, lowest prices
	"us-east-2",      // Ohio - Large capacity, competitive
	"us-west-2",      // Oregon - Second largest US
	"eu-west-1",      // Ireland - Largest EU
	"eu-central-1",   // Frankfurt - Good EU alternative
	"ap-northeast-1", // Tokyo - Largest APAC
	"ap-southeast-1", // Singapore - Good APAC pricing
	"ap-south-1",     // Mumbai - Large capacity
	"ca-central-1",   // Canada - Often good prices
}

// regionLocations maps AWS region codes to Pricing API location names.
// Only includes supported regions for benchmarks.
var regionLocations = map[string]string{
	"us-east-1":      "US East (N. Virginia)",
	"us-east-2":      "US East (Ohio)",
	"us-west-2":      "US West (Oregon)",
	"eu-west-1":      "EU (Ireland)",
	"eu-central-1":   "EU (Frankfurt)",
	"ap-northeast-1": "Asia Pacific (Tokyo)",
	"ap-southeast-1": "Asia Pacific (Singapore)",
	"ap-south-1":     "Asia Pacific (Mumbai)",
	"ca-central-1":   "Canada (Central)",
}

// SpotPrice represents spot pricing for an instance type in an AZ.
type SpotPrice struct {
	InstanceType string
	AZ           string
	Price        float64
	Timestamp    time.Time
}

// BidResult contains the recommended bid and AZ for an instance type.
type BidResult struct {
	InstanceType  string
	AZ            string
	CurrentPrice  float64
	BidPrice      float64
	OnDemandPrice float64
}

// InstancePair holds server and client instance types.
type InstancePair struct {
	Server string
	Client string
}

// Instance types for each mode and architecture (primary)
var InstanceTypes = map[string]map[string]InstancePair{
	"fast": {
		"arm64": {Server: "c6g.medium", Client: "t4g.small"},
		"x86":   {Server: "c5.large", Client: "t3.small"},
	},
	"med": {
		"arm64": {Server: "c6g.2xlarge", Client: "c6g.xlarge"},
		"x86":   {Server: "c5.2xlarge", Client: "c5.xlarge"},
	},
	"metal": {
		"arm64": {Server: "c6g.metal", Client: "c6g.4xlarge"},
		"x86":   {Server: "c5.metal", Client: "c5.9xlarge"},
	},
}

// Alternative instance types (Alt1) - must match workers.yaml InstanceTypesAlt1
var InstanceTypesAlt1 = map[string]map[string]InstancePair{
	"fast": {
		"arm64": {Server: "c7g.medium", Client: "t4g.micro"},
		"x86":   {Server: "c5a.large", Client: "t3a.small"},
	},
	"med": {
		"arm64": {Server: "c7g.2xlarge", Client: "c7g.xlarge"},
		"x86":   {Server: "c5a.2xlarge", Client: "c5a.xlarge"},
	},
	"metal": {
		"arm64": {Server: "c7g.metal", Client: "c7g.4xlarge"},
		"x86":   {Server: "c5d.metal", Client: "c5d.9xlarge"},
	},
}

// Alternative instance types (Alt2) - must match workers.yaml InstanceTypesAlt2
var InstanceTypesAlt2 = map[string]map[string]InstancePair{
	"fast": {
		"arm64": {Server: "c6g.large", Client: "t4g.medium"},
		"x86":   {Server: "c6i.large", Client: "t2.small"},
	},
	"med": {
		"arm64": {Server: "c6g.4xlarge", Client: "c6g.2xlarge"},
		"x86":   {Server: "c6i.2xlarge", Client: "c6i.xlarge"},
	},
	"metal": {
		"arm64": {Server: "m7g.metal", Client: "c6g.8xlarge"},
		"x86":   {Server: "c5n.metal", Client: "c5n.9xlarge"},
	},
}

// NewClient creates a new spot client.
func NewClient(region string) (*Client, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Pricing API is only available in us-east-1
	pricingCfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion("us-east-1"))
	if err != nil {
		return nil, fmt.Errorf("failed to load pricing config: %w", err)
	}

	return &Client{
		ec2:        ec2.NewFromConfig(cfg),
		pricing:    pricing.NewFromConfig(pricingCfg),
		region:     region,
		priceCache: make(map[string]*cachedPrice),
	}, nil
}

// RegionQuota holds quota information for a region.
type RegionQuota struct {
	Region        string
	QuotaLimit    int // Total vCPU quota for spot instances
	QuotaUsed     int // Currently used vCPUs (estimated from running instances)
	QuotaAvail    int // Available quota (limit - used)
	HasSufficent  bool
	VCPUsRequired int
}

// RegionResult holds the best pricing result for a region.
type RegionResult struct {
	Region      string
	AZ          string
	ServerPrice float64
	ClientPrice float64
	TotalPrice  float64
	Score       int // Placement score (1-10, higher is better)
	Available   bool
	Quota       *RegionQuota
}

// ArchitectureSelection holds the selected region/AZ for one architecture.
type ArchitectureSelection struct {
	Arch       string
	Region     string
	AZ         string
	ServerType string
	ClientType string
	ServerBid  *BidResult
	ClientBid  *BidResult
	VCPUs      int
}

// GetVCPUsForPair returns the total vCPUs needed for a server+client pair.
func GetVCPUsForPair(serverType, clientType string) int {
	serverVCPUs := instanceVCPUs[serverType]
	clientVCPUs := instanceVCPUs[clientType]
	if serverVCPUs == 0 {
		slog.Warn("unknown vCPU count for instance type, assuming 64", "type", serverType)
		serverVCPUs = 64 // Conservative default for unknown types
	}
	if clientVCPUs == 0 {
		slog.Warn("unknown vCPU count for instance type, assuming 64", "type", clientType)
		clientVCPUs = 64
	}
	return serverVCPUs + clientVCPUs
}

// getRegionQuota checks the spot instance quota for a region.
func getRegionQuota(ctx context.Context, region string, vcpusRequired int) *RegionQuota {
	quota := &RegionQuota{
		Region:        region,
		VCPUsRequired: vcpusRequired,
		HasSufficent:  false,
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		slog.Debug("failed to load config for quota check", "region", region, "error", err)
		return quota
	}

	sqClient := servicequotas.NewFromConfig(cfg)

	// Get the quota limit
	result, err := awsutil.WithRetry(ctx, "GetServiceQuota", awsutil.DefaultMaxRetries, func() (*servicequotas.GetServiceQuotaOutput, error) {
		return sqClient.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{
			ServiceCode: strPtr("ec2"),
			QuotaCode:   strPtr(SpotQuotaCode),
		})
	})
	if err != nil {
		slog.Debug("failed to get quota", "region", region, "error", err)
		// If we can't check quota, assume it's not available
		return quota
	}

	if result.Quota != nil && result.Quota.Value != nil {
		quota.QuotaLimit = int(*result.Quota.Value)
	}

	// Estimate used quota by counting running spot instances
	// This is an approximation - actual usage tracking would require CloudWatch
	ec2Client := ec2.NewFromConfig(cfg)
	instances, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: strPtr("instance-lifecycle"), Values: []string{"spot"}},
			{Name: strPtr("instance-state-name"), Values: []string{"pending", "running"}},
			{Name: strPtr("tag:Project"), Values: []string{"celeris-benchmarks"}},
		},
	})
	if err == nil {
		for _, res := range instances.Reservations {
			for _, inst := range res.Instances {
				if vcpus, ok := instanceVCPUs[string(inst.InstanceType)]; ok {
					quota.QuotaUsed += vcpus
				}
			}
		}
	}

	quota.QuotaAvail = quota.QuotaLimit - quota.QuotaUsed
	quota.HasSufficent = quota.QuotaAvail >= vcpusRequired

	slog.Debug("region quota check",
		"region", region,
		"limit", quota.QuotaLimit,
		"used", quota.QuotaUsed,
		"available", quota.QuotaAvail,
		"required", vcpusRequired,
		"sufficient", quota.HasSufficent)

	return quota
}

// FindBestRegionsForBenchmark finds the best regions for both architectures,
// ensuring quota is properly allocated between them.
// Returns selections for arm64 and x86, which may be in the same or different regions.
func FindBestRegionsForBenchmark(ctx context.Context, mode string) (arm64 *ArchitectureSelection, x86 *ArchitectureSelection, err error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	// Get instance types for both architectures
	arm64Server, arm64Client, err := GetInstancesForMode(mode, "arm64")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get arm64 instances: %w", err)
	}
	x86Server, x86Client, err := GetInstancesForMode(mode, "x86")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get x86 instances: %w", err)
	}

	arm64VCPUs := GetVCPUsForPair(arm64Server, arm64Client)
	x86VCPUs := GetVCPUsForPair(x86Server, x86Client)
	totalVCPUs := arm64VCPUs + x86VCPUs

	slog.Info("calculating vCPU requirements",
		"mode", mode,
		"arm64_vcpus", arm64VCPUs,
		"x86_vcpus", x86VCPUs,
		"total_vcpus", totalVCPUs)

	// Step 1: Check quotas in all regions in parallel
	type regionQuotaResult struct {
		region string
		quota  *RegionQuota
	}
	var (
		quotaResults []regionQuotaResult
		quotaMu      sync.Mutex
		quotaWg      sync.WaitGroup
	)

	for _, region := range SupportedRegions {
		quotaWg.Add(1)
		go func(region string) {
			defer quotaWg.Done()
			// Check if region has enough for BOTH architectures
			quota := getRegionQuota(ctx, region, totalVCPUs)
			quotaMu.Lock()
			quotaResults = append(quotaResults, regionQuotaResult{region, quota})
			quotaMu.Unlock()
		}(region)
	}
	quotaWg.Wait()

	// Separate regions into those that can handle both vs. single architecture
	var regionsBoth, regionsSingle []string
	for _, r := range quotaResults {
		if r.quota.QuotaAvail >= totalVCPUs {
			regionsBoth = append(regionsBoth, r.region)
			slog.Debug("region can handle both architectures",
				"region", r.region,
				"available", r.quota.QuotaAvail,
				"required", totalVCPUs)
		} else if r.quota.QuotaAvail >= arm64VCPUs || r.quota.QuotaAvail >= x86VCPUs {
			regionsSingle = append(regionsSingle, r.region)
			slog.Debug("region can handle single architecture",
				"region", r.region,
				"available", r.quota.QuotaAvail)
		}
	}

	slog.Info("quota check complete",
		"regions_both", len(regionsBoth),
		"regions_single", len(regionsSingle))

	// Step 2: Try to find a single region for both (preferred)
	if len(regionsBoth) > 0 {
		arm64Sel, x86Sel, err := findBestRegionForBoth(ctx, regionsBoth, mode, arm64Server, arm64Client, x86Server, x86Client)
		if err == nil {
			return arm64Sel, x86Sel, nil
		}
		slog.Warn("failed to find single region for both, trying split", "error", err)
	}

	// Step 3: Fall back to separate regions for each architecture
	allRegions := append(regionsBoth, regionsSingle...)
	if len(allRegions) == 0 {
		return nil, nil, fmt.Errorf("no regions have sufficient quota for benchmarks (need %d vCPUs)", totalVCPUs)
	}

	// Find best region for arm64
	arm64Sel, err := findBestRegionForArch(ctx, allRegions, "arm64", arm64Server, arm64Client, arm64VCPUs)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find region for arm64: %w", err)
	}

	// Find best region for x86 (excluding arm64's region if quota is tight)
	x86Regions := allRegions
	// Check if arm64's region can also fit x86
	for _, r := range quotaResults {
		if r.region == arm64Sel.Region && r.quota.QuotaAvail < totalVCPUs {
			// Need to use a different region for x86
			x86Regions = make([]string, 0, len(allRegions)-1)
			for _, reg := range allRegions {
				if reg != arm64Sel.Region {
					x86Regions = append(x86Regions, reg)
				}
			}
			break
		}
	}

	x86Sel, err := findBestRegionForArch(ctx, x86Regions, "x86", x86Server, x86Client, x86VCPUs)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find region for x86: %w", err)
	}

	return arm64Sel, x86Sel, nil
}

// findBestRegionForBoth finds the best single region that can run both architectures.
func findBestRegionForBoth(ctx context.Context, regions []string, mode, arm64Server, arm64Client, x86Server, x86Client string) (*ArchitectureSelection, *ArchitectureSelection, error) {
	type combinedResult struct {
		region      string
		arm64Result RegionResult
		x86Result   RegionResult
		totalPrice  float64
		minScore    int
	}

	var (
		results []combinedResult
		mu      sync.Mutex
		wg      sync.WaitGroup
	)

	for _, region := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()

			arm64Res := queryRegionPrices(ctx, region, arm64Server, arm64Client)
			x86Res := queryRegionPrices(ctx, region, x86Server, x86Client)

			if arm64Res.Available && x86Res.Available {
				minScore := arm64Res.Score
				if x86Res.Score < minScore {
					minScore = x86Res.Score
				}

				mu.Lock()
				results = append(results, combinedResult{
					region:      region,
					arm64Result: arm64Res,
					x86Result:   x86Res,
					totalPrice:  arm64Res.TotalPrice + x86Res.TotalPrice,
					minScore:    minScore,
				})
				mu.Unlock()
			}
		}(region)
	}
	wg.Wait()

	if len(results) == 0 {
		return nil, nil, fmt.Errorf("no region found with availability for both architectures")
	}

	// Sort by score (desc) then total price (asc)
	sort.Slice(results, func(i, j int) bool {
		if results[i].minScore != results[j].minScore {
			return results[i].minScore > results[j].minScore
		}
		return results[i].totalPrice < results[j].totalPrice
	})

	best := results[0]
	slog.Info("selected single region for both architectures",
		"region", best.region,
		"arm64_az", best.arm64Result.AZ,
		"x86_az", best.x86Result.AZ,
		"total_price", best.totalPrice,
		"score", best.minScore)

	// Create client for bid calculations
	client, err := NewClient(best.region)
	if err != nil {
		return nil, nil, err
	}

	arm64Sel := createArchSelection(ctx, client, "arm64", best.region, best.arm64Result, arm64Server, arm64Client)
	x86Sel := createArchSelection(ctx, client, "x86", best.region, best.x86Result, x86Server, x86Client)

	return arm64Sel, x86Sel, nil
}

// findBestRegionForArch finds the best region for a single architecture.
func findBestRegionForArch(ctx context.Context, regions []string, arch, serverType, clientType string, vcpusRequired int) (*ArchitectureSelection, error) {
	var (
		results []RegionResult
		mu      sync.Mutex
		wg      sync.WaitGroup
	)

	for _, region := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			result := queryRegionPrices(ctx, region, serverType, clientType)
			if result.Available {
				// Also verify quota for this specific architecture
				quota := getRegionQuota(ctx, region, vcpusRequired)
				if quota.HasSufficent {
					result.Quota = quota
					mu.Lock()
					results = append(results, result)
					mu.Unlock()
				}
			}
		}(region)
	}
	wg.Wait()

	if len(results) == 0 {
		return nil, fmt.Errorf("no region found with availability and quota for %s", arch)
	}

	// Sort by score (desc) then price (asc)
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].TotalPrice < results[j].TotalPrice
	})

	best := results[0]
	client, err := NewClient(best.Region)
	if err != nil {
		return nil, err
	}

	return createArchSelection(ctx, client, arch, best.Region, best, serverType, clientType), nil
}

// createArchSelection creates an ArchitectureSelection from a RegionResult.
func createArchSelection(ctx context.Context, client *Client, arch, region string, result RegionResult, serverType, clientType string) *ArchitectureSelection {
	serverOnDemand, _ := client.getOnDemandPrice(ctx, serverType)
	clientOnDemand, _ := client.getOnDemandPrice(ctx, clientType)

	serverBid := result.ServerPrice * 1.50
	if serverOnDemand > 0 && serverBid > serverOnDemand {
		serverBid = serverOnDemand
	}

	clientBid := result.ClientPrice * 1.50
	if clientOnDemand > 0 && clientBid > clientOnDemand {
		clientBid = clientOnDemand
	}

	return &ArchitectureSelection{
		Arch:       arch,
		Region:     region,
		AZ:         result.AZ,
		ServerType: serverType,
		ClientType: clientType,
		VCPUs:      GetVCPUsForPair(serverType, clientType),
		ServerBid: &BidResult{
			InstanceType:  serverType,
			AZ:            result.AZ,
			CurrentPrice:  result.ServerPrice,
			BidPrice:      serverBid,
			OnDemandPrice: serverOnDemand,
		},
		ClientBid: &BidResult{
			InstanceType:  clientType,
			AZ:            result.AZ,
			CurrentPrice:  result.ClientPrice,
			BidPrice:      clientBid,
			OnDemandPrice: clientOnDemand,
		},
	}
}

// FindBestRegionForPair queries all supported regions in parallel to find the
// cheapest region with availability for both server and client instance types.
// Returns the best region, AZ, and bid results for both instances.
// NOTE: This function does not check quotas. Use FindBestRegionsForBenchmark for quota-aware selection.
func FindBestRegionForPair(ctx context.Context, serverType, clientType string) (string, string, *BidResult, *BidResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var (
		results []RegionResult
		mu      sync.Mutex
		wg      sync.WaitGroup
	)

	// Query all supported regions in parallel
	for _, region := range SupportedRegions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()

			result := queryRegionPrices(ctx, region, serverType, clientType)
			if result.Available {
				mu.Lock()
				results = append(results, result)
				mu.Unlock()
			}
		}(region)
	}

	wg.Wait()

	if len(results) == 0 {
		return "", "", nil, nil, fmt.Errorf("no region found with availability for %s and %s", serverType, clientType)
	}

	// Sort by: availability score (desc), then total price (asc)
	sort.Slice(results, func(i, j int) bool {
		// First prioritize higher availability scores
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		// Then lower price
		return results[i].TotalPrice < results[j].TotalPrice
	})

	best := results[0]
	slog.Info("selected best region for benchmark",
		"region", best.Region,
		"az", best.AZ,
		"server_price", best.ServerPrice,
		"client_price", best.ClientPrice,
		"total_price", best.TotalPrice,
		"score", best.Score,
		"alternatives", len(results)-1)

	// Log alternatives for debugging
	if len(results) > 1 {
		for i, r := range results[1:] {
			if i >= 3 { // Only log top 3 alternatives
				break
			}
			slog.Debug("alternative region",
				"region", r.Region,
				"az", r.AZ,
				"total_price", r.TotalPrice,
				"score", r.Score)
		}
	}

	// Create a client for the best region to get on-demand prices
	client, err := NewClient(best.Region)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("failed to create client for %s: %w", best.Region, err)
	}

	serverOnDemand, _ := client.getOnDemandPrice(ctx, serverType)
	clientOnDemand, _ := client.getOnDemandPrice(ctx, clientType)

	// Calculate bids (50% above spot, capped at on-demand)
	serverBid := best.ServerPrice * 1.50
	if serverOnDemand > 0 && serverBid > serverOnDemand {
		serverBid = serverOnDemand
	}

	clientBid := best.ClientPrice * 1.50
	if clientOnDemand > 0 && clientBid > clientOnDemand {
		clientBid = clientOnDemand
	}

	return best.Region, best.AZ, &BidResult{
			InstanceType:  serverType,
			AZ:            best.AZ,
			CurrentPrice:  best.ServerPrice,
			BidPrice:      serverBid,
			OnDemandPrice: serverOnDemand,
		}, &BidResult{
			InstanceType:  clientType,
			AZ:            best.AZ,
			CurrentPrice:  best.ClientPrice,
			BidPrice:      clientBid,
			OnDemandPrice: clientOnDemand,
		}, nil
}

// queryRegionPrices queries spot prices and availability for a single region.
func queryRegionPrices(ctx context.Context, region, serverType, clientType string) RegionResult {
	result := RegionResult{
		Region:    region,
		Available: false,
		Score:     0,
	}

	// Create a client for this region
	client, err := NewClient(region)
	if err != nil {
		slog.Debug("failed to create client for region", "region", region, "error", err)
		return result
	}

	// Get spot prices for both instance types
	serverPrices, err := client.GetSpotPrices(ctx, serverType)
	if err != nil || len(serverPrices) == 0 {
		slog.Debug("no server prices in region", "region", region, "server_type", serverType)
		return result
	}

	clientPrices, err := client.GetSpotPrices(ctx, clientType)
	if err != nil || len(clientPrices) == 0 {
		slog.Debug("no client prices in region", "region", region, "client_type", clientType)
		return result
	}

	// Build client price map by AZ
	clientPriceMap := make(map[string]float64)
	for _, p := range clientPrices {
		clientPriceMap[p.AZ] = p.Price
	}

	// Find best AZ where both are available
	var bestAZ string
	var bestServerPrice, bestClientPrice float64
	bestTotal := math.MaxFloat64

	for _, sp := range serverPrices {
		if cp, ok := clientPriceMap[sp.AZ]; ok {
			total := sp.Price + cp
			if total < bestTotal {
				bestTotal = total
				bestAZ = sp.AZ
				bestServerPrice = sp.Price
				bestClientPrice = cp
			}
		}
	}

	if bestAZ == "" {
		slog.Debug("no common AZ in region", "region", region)
		return result
	}

	// Try to get placement scores for availability
	score := 5 // Default score if we can't get placement scores
	scores, err := client.getSpotPlacementScores(ctx, []string{serverType, clientType}, []string{bestAZ})
	if err == nil && len(scores) > 0 {
		if azScores, ok := scores[bestAZ]; ok {
			// Use minimum score across both instance types
			minScore := 10
			for _, s := range azScores {
				if s < minScore {
					minScore = s
				}
			}
			score = minScore
		}
	}

	result.AZ = bestAZ
	result.ServerPrice = bestServerPrice
	result.ClientPrice = bestClientPrice
	result.TotalPrice = bestTotal
	result.Score = score
	result.Available = true

	slog.Debug("region query complete",
		"region", region,
		"az", bestAZ,
		"total_price", bestTotal,
		"score", score)

	return result
}

// GetSpotPrices retrieves current spot prices for an instance type across all AZs.
func (c *Client) GetSpotPrices(ctx context.Context, instanceType string) ([]SpotPrice, error) {
	// Get spot price history (last hour gives current prices)
	startTime := time.Now().Add(-1 * time.Hour)

	input := &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []types.InstanceType{types.InstanceType(instanceType)},
		ProductDescriptions: []string{"Linux/UNIX"},
		StartTime:           &startTime,
	}

	result, err := awsutil.WithRetry(ctx, "DescribeSpotPriceHistory", awsutil.DefaultMaxRetries, func() (*ec2.DescribeSpotPriceHistoryOutput, error) {
		return c.ec2.DescribeSpotPriceHistory(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get spot prices: %w", err)
	}

	// Deduplicate by AZ (keep most recent)
	priceMap := make(map[string]SpotPrice)
	for _, sp := range result.SpotPriceHistory {
		az := *sp.AvailabilityZone
		price := parsePrice(*sp.SpotPrice)

		existing, ok := priceMap[az]
		if !ok || sp.Timestamp.After(existing.Timestamp) {
			priceMap[az] = SpotPrice{
				InstanceType: instanceType,
				AZ:           az,
				Price:        price,
				Timestamp:    *sp.Timestamp,
			}
		}
	}

	var prices []SpotPrice
	for _, p := range priceMap {
		prices = append(prices, p)
	}

	// Sort by price (cheapest first)
	sort.Slice(prices, func(i, j int) bool {
		return prices[i].Price < prices[j].Price
	})

	return prices, nil
}

// GetBestAZ returns the cheapest AZ for an instance type with a recommended bid.
func (c *Client) GetBestAZ(ctx context.Context, instanceType string) (*BidResult, error) {
	prices, err := c.GetSpotPrices(ctx, instanceType)
	if err != nil {
		return nil, err
	}

	if len(prices) == 0 {
		return nil, fmt.Errorf("no spot prices found for %s", instanceType)
	}

	// Get on-demand price for cap
	onDemandPrice, err := c.getOnDemandPrice(ctx, instanceType)
	if err != nil {
		log.Printf("Warning: Could not get on-demand price for %s: %v", instanceType, err)
		onDemandPrice = prices[0].Price * 3 // Fallback: 3x spot
	}

	// Bid 20% above current spot price, capped at on-demand
	bestPrice := prices[0]
	bidPrice := bestPrice.Price * 1.50
	if bidPrice > onDemandPrice {
		bidPrice = onDemandPrice
	}

	return &BidResult{
		InstanceType:  instanceType,
		AZ:            bestPrice.AZ,
		CurrentPrice:  bestPrice.Price,
		BidPrice:      bidPrice,
		OnDemandPrice: onDemandPrice,
	}, nil
}

// AZCapacity holds capacity scores for an AZ.
type AZCapacity struct {
	AZ          string
	Score       int // 1-10, higher is better capacity
	ServerPrice float64
	ClientPrice float64
	TotalPrice  float64
}

// GetBestAZForPair finds the best AZ for a server+client pair.
// Both must be in the same AZ, so we pick the cheapest for the larger instance.
// NOTE: This method only considers price, not capacity. Use GetBestAZWithCapacity for capacity-aware selection.
func (c *Client) GetBestAZForPair(ctx context.Context, serverType, clientType string) (*BidResult, *BidResult, error) {
	// Get prices for server (usually larger, so optimize for that)
	serverPrices, err := c.GetSpotPrices(ctx, serverType)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get server prices: %w", err)
	}

	if len(serverPrices) == 0 {
		return nil, nil, fmt.Errorf("no spot prices found for %s", serverType)
	}

	// Get client prices for the same AZs
	clientPrices, err := c.GetSpotPrices(ctx, clientType)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get client prices: %w", err)
	}

	clientPriceMap := make(map[string]float64)
	for _, p := range clientPrices {
		clientPriceMap[p.AZ] = p.Price
	}

	// Find best AZ where both are available
	var bestAZ string
	var bestServerPrice, bestClientPrice float64
	bestTotal := math.MaxFloat64

	for _, sp := range serverPrices {
		if cp, ok := clientPriceMap[sp.AZ]; ok {
			total := sp.Price + cp
			if total < bestTotal {
				bestTotal = total
				bestAZ = sp.AZ
				bestServerPrice = sp.Price
				bestClientPrice = cp
			}
		}
	}

	if bestAZ == "" {
		return nil, nil, fmt.Errorf("no AZ found with both %s and %s available", serverType, clientType)
	}

	// Calculate bids (20% above current, capped at on-demand)
	serverOnDemand, _ := c.getOnDemandPrice(ctx, serverType)
	clientOnDemand, _ := c.getOnDemandPrice(ctx, clientType)

	serverBid := bestServerPrice * 1.50
	if serverOnDemand > 0 && serverBid > serverOnDemand {
		serverBid = serverOnDemand
	}

	clientBid := bestClientPrice * 1.50
	if clientOnDemand > 0 && clientBid > clientOnDemand {
		clientBid = clientOnDemand
	}

	return &BidResult{
			InstanceType:  serverType,
			AZ:            bestAZ,
			CurrentPrice:  bestServerPrice,
			BidPrice:      serverBid,
			OnDemandPrice: serverOnDemand,
		}, &BidResult{
			InstanceType:  clientType,
			AZ:            bestAZ,
			CurrentPrice:  bestClientPrice,
			BidPrice:      clientBid,
			OnDemandPrice: clientOnDemand,
		}, nil
}

// GetBestAZWithCapacity finds the best AZ considering both capacity AND price.
// It checks placement scores for all instance types (primary + alternates) to ensure
// at least one server and one client type has capacity in the selected AZ.
func (c *Client) GetBestAZWithCapacity(ctx context.Context, mode, arch string, availableAZs []string) (string, *BidResult, *BidResult, error) {
	// Get all instance types we need to check
	servers, clients, err := GetAllInstanceTypesForMode(mode, arch)
	if err != nil {
		return "", nil, nil, err
	}

	log.Printf("Checking capacity for %s %s: servers=%v, clients=%v", mode, arch, servers, clients)

	// Get placement scores for all instance types
	allTypes := append(servers, clients...)
	scores, err := c.getSpotPlacementScores(ctx, allTypes, availableAZs)
	if err != nil {
		log.Printf("Warning: Could not get placement scores, falling back to price-only selection: %v", err)
		// Fall back to price-only selection with primary types
		serverBid, clientBid, err := c.GetBestAZForPair(ctx, servers[0], clients[0])
		if err != nil {
			return "", nil, nil, err
		}
		return serverBid.AZ, serverBid, clientBid, nil
	}

	// Score each AZ by checking if it has capacity for at least one server AND one client type
	azScores := make(map[string]*AZCapacity)
	for az := range scores {
		// Check if any server type has good capacity (score >= 5)
		serverHasCapacity := false
		for _, serverType := range servers {
			if score, ok := scores[az][serverType]; ok && score >= 5 {
				serverHasCapacity = true
				break
			}
		}

		// Check if any client type has good capacity (score >= 5)
		clientHasCapacity := false
		for _, clientType := range clients {
			if score, ok := scores[az][clientType]; ok && score >= 5 {
				clientHasCapacity = true
				break
			}
		}

		if serverHasCapacity && clientHasCapacity {
			// Calculate minimum score across all types as AZ score
			minScore := 10
			for _, instanceType := range allTypes {
				if score, ok := scores[az][instanceType]; ok && score < minScore {
					minScore = score
				}
			}
			azScores[az] = &AZCapacity{
				AZ:    az,
				Score: minScore,
			}
		}
	}

	if len(azScores) == 0 {
		log.Printf("Warning: No AZ has capacity for both server and client types, trying all AZs")
		// If no AZ has good capacity for both, try all available AZs
		for _, az := range availableAZs {
			azScores[az] = &AZCapacity{AZ: az, Score: 1}
		}
	}

	// Get prices for primary types in qualifying AZs
	serverPrices, err := c.GetSpotPrices(ctx, servers[0])
	if err != nil {
		return "", nil, nil, fmt.Errorf("failed to get server prices: %w", err)
	}

	clientPrices, err := c.GetSpotPrices(ctx, clients[0])
	if err != nil {
		return "", nil, nil, fmt.Errorf("failed to get client prices: %w", err)
	}

	// Build price maps
	serverPriceMap := make(map[string]float64)
	for _, p := range serverPrices {
		serverPriceMap[p.AZ] = p.Price
	}

	clientPriceMap := make(map[string]float64)
	for _, p := range clientPrices {
		clientPriceMap[p.AZ] = p.Price
	}

	// Add prices to AZ scores
	for az, cap := range azScores {
		if sp, ok := serverPriceMap[az]; ok {
			cap.ServerPrice = sp
		}
		if cp, ok := clientPriceMap[az]; ok {
			cap.ClientPrice = cp
		}
		cap.TotalPrice = cap.ServerPrice + cap.ClientPrice
	}

	// Sort AZs: first by capacity score (descending), then by price (ascending)
	var sortedAZs []*AZCapacity
	for _, cap := range azScores {
		if cap.ServerPrice > 0 && cap.ClientPrice > 0 { // Must have prices
			sortedAZs = append(sortedAZs, cap)
		}
	}

	if len(sortedAZs) == 0 {
		return "", nil, nil, fmt.Errorf("no AZ found with capacity and pricing for %s %s", mode, arch)
	}

	sort.Slice(sortedAZs, func(i, j int) bool {
		// Prioritize higher capacity scores
		if sortedAZs[i].Score != sortedAZs[j].Score {
			return sortedAZs[i].Score > sortedAZs[j].Score
		}
		// Then lower price
		return sortedAZs[i].TotalPrice < sortedAZs[j].TotalPrice
	})

	// Select best AZ
	best := sortedAZs[0]
	log.Printf("Selected AZ %s for %s %s (score: %d, server: $%.4f, client: $%.4f)",
		best.AZ, mode, arch, best.Score, best.ServerPrice, best.ClientPrice)

	// Log alternatives for debugging
	if len(sortedAZs) > 1 {
		log.Printf("Alternative AZs: %v", sortedAZs[1:])
	}

	// Build bid results
	serverOnDemand, _ := c.getOnDemandPrice(ctx, servers[0])
	clientOnDemand, _ := c.getOnDemandPrice(ctx, clients[0])

	serverBid := best.ServerPrice * 1.50
	if serverOnDemand > 0 && serverBid > serverOnDemand {
		serverBid = serverOnDemand
	}

	clientBid := best.ClientPrice * 1.50
	if clientOnDemand > 0 && clientBid > clientOnDemand {
		clientBid = clientOnDemand
	}

	return best.AZ, &BidResult{
			InstanceType:  servers[0],
			AZ:            best.AZ,
			CurrentPrice:  best.ServerPrice,
			BidPrice:      serverBid,
			OnDemandPrice: serverOnDemand,
		}, &BidResult{
			InstanceType:  clients[0],
			AZ:            best.AZ,
			CurrentPrice:  best.ClientPrice,
			BidPrice:      clientBid,
			OnDemandPrice: clientOnDemand,
		}, nil
}

// getSpotPlacementScores retrieves placement scores for instance types across AZs.
// Returns map[AZName][InstanceType]Score where Score is 1-10 (higher = better capacity).
func (c *Client) getSpotPlacementScores(ctx context.Context, instanceTypes []string, targetAZs []string) (map[string]map[string]int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// First, get AZ ID to name mapping (API returns IDs like use1-az1, we need names like us-east-1a)
	azIDToName, err := c.getAZIDToNameMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get AZ mapping: %w", err)
	}

	// Build regional request
	input := &ec2.GetSpotPlacementScoresInput{
		InstanceTypes:          instanceTypes,
		TargetCapacity:         intPtr(1),
		SingleAvailabilityZone: boolPtr(true),
		RegionNames:            []string{c.region},
	}

	result, err := awsutil.WithRetry(ctx, "GetSpotPlacementScores", awsutil.DefaultMaxRetries, func() (*ec2.GetSpotPlacementScoresOutput, error) {
		return c.ec2.GetSpotPlacementScores(ctx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get placement scores: %w", err)
	}

	// Build target AZ set for filtering
	targetAZSet := make(map[string]bool)
	for _, az := range targetAZs {
		targetAZSet[az] = true
	}

	// Build result map using AZ names (not IDs)
	scores := make(map[string]map[string]int)

	for _, score := range result.SpotPlacementScores {
		if score.AvailabilityZoneId == nil || score.Score == nil {
			continue
		}

		// Convert AZ ID to AZ name (e.g., use1-az1 -> us-east-1a)
		azID := *score.AvailabilityZoneId
		azName, ok := azIDToName[azID]
		if !ok {
			log.Printf("Warning: Unknown AZ ID %s, skipping", azID)
			continue
		}

		// Only include target AZs
		if len(targetAZs) > 0 && !targetAZSet[azName] {
			continue
		}

		if scores[azName] == nil {
			scores[azName] = make(map[string]int)
		}

		// Score applies to all instance types in the request
		for _, t := range instanceTypes {
			scores[azName][t] = int(*score.Score)
		}

		log.Printf("Placement score for %s: %d (types: %v)", azName, *score.Score, instanceTypes)
	}

	return scores, nil
}

// getAZIDToNameMap returns a mapping of AZ IDs to AZ names.
func (c *Client) getAZIDToNameMap(ctx context.Context) (map[string]string, error) {
	result, err := c.ec2.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{})
	if err != nil {
		return nil, err
	}

	azMap := make(map[string]string)
	for _, az := range result.AvailabilityZones {
		if az.ZoneId != nil && az.ZoneName != nil {
			azMap[*az.ZoneId] = *az.ZoneName
		}
	}

	return azMap, nil
}

// GetPricesForAZ gets spot prices for a server+client pair in a specific AZ.
// If spot capacity is unavailable, returns on-demand prices as fallback.
func (c *Client) GetPricesForAZ(ctx context.Context, serverType, clientType, az string) (*BidResult, *BidResult, error) {
	// Get spot prices for server in specific AZ
	serverPrices, err := c.GetSpotPrices(ctx, serverType)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get server prices: %w", err)
	}

	// Get spot prices for client in specific AZ
	clientPrices, err := c.GetSpotPrices(ctx, clientType)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get client prices: %w", err)
	}

	// Find prices for the specified AZ
	var serverPrice, clientPrice float64
	var serverFound, clientFound bool

	for _, p := range serverPrices {
		if p.AZ == az {
			serverPrice = p.Price
			serverFound = true
			break
		}
	}

	for _, p := range clientPrices {
		if p.AZ == az {
			clientPrice = p.Price
			clientFound = true
			break
		}
	}

	// Get on-demand prices
	serverOnDemand, _ := c.getOnDemandPrice(ctx, serverType)
	clientOnDemand, _ := c.getOnDemandPrice(ctx, clientType)

	// If spot not available, use on-demand price
	if !serverFound {
		log.Printf("Warning: No spot price for %s in %s, using on-demand: $%.4f", serverType, az, serverOnDemand)
		serverPrice = serverOnDemand
	}
	if !clientFound {
		log.Printf("Warning: No spot price for %s in %s, using on-demand: $%.4f", clientType, az, clientOnDemand)
		clientPrice = clientOnDemand
	}

	// Calculate bids (20% above current, capped at on-demand)
	serverBid := serverPrice * 1.50
	if serverOnDemand > 0 && serverBid > serverOnDemand {
		serverBid = serverOnDemand
	}

	clientBid := clientPrice * 1.50
	if clientOnDemand > 0 && clientBid > clientOnDemand {
		clientBid = clientOnDemand
	}

	return &BidResult{
			InstanceType:  serverType,
			AZ:            az,
			CurrentPrice:  serverPrice,
			BidPrice:      serverBid,
			OnDemandPrice: serverOnDemand,
		}, &BidResult{
			InstanceType:  clientType,
			AZ:            az,
			CurrentPrice:  clientPrice,
			BidPrice:      clientBid,
			OnDemandPrice: clientOnDemand,
		}, nil
}

// GetInstancesForMode returns the primary instance types for a benchmark mode and architecture.
func GetInstancesForMode(mode, arch string) (serverType, clientType string, err error) {
	modeTypes, ok := InstanceTypes[mode]
	if !ok {
		return "", "", fmt.Errorf("unknown mode: %s", mode)
	}

	archTypes, ok := modeTypes[arch]
	if !ok {
		return "", "", fmt.Errorf("unknown architecture: %s", arch)
	}

	return archTypes.Server, archTypes.Client, nil
}

// GetAllInstanceTypesForMode returns all instance types (primary + alternates) for capacity checking.
// Returns servers and clients as separate slices.
func GetAllInstanceTypesForMode(mode, arch string) (servers, clients []string, err error) {
	typeMaps := []map[string]map[string]InstancePair{InstanceTypes, InstanceTypesAlt1, InstanceTypesAlt2}

	for _, typeMap := range typeMaps {
		if modeTypes, ok := typeMap[mode]; ok {
			if archTypes, ok := modeTypes[arch]; ok {
				servers = append(servers, archTypes.Server)
				clients = append(clients, archTypes.Client)
			}
		}
	}

	if len(servers) == 0 {
		return nil, nil, fmt.Errorf("unknown mode/arch: %s/%s", mode, arch)
	}

	return servers, clients, nil
}

// DescribeInstances returns instances matching the given filters.
func (c *Client) DescribeInstances(ctx context.Context, runID string) ([]types.Instance, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   strPtr("tag:RunId"),
				Values: []string{runID},
			},
			{
				Name:   strPtr("instance-state-name"),
				Values: []string{"pending", "running"},
			},
		},
	}

	result, err := awsutil.WithRetry(ctx, "DescribeInstances", awsutil.DefaultMaxRetries, func() (*ec2.DescribeInstancesOutput, error) {
		return c.ec2.DescribeInstances(ctx, input)
	})
	if err != nil {
		return nil, err
	}

	var instances []types.Instance
	for _, res := range result.Reservations {
		instances = append(instances, res.Instances...)
	}

	return instances, nil
}

// TerminateInstances terminates the given instances.
func (c *Client) TerminateInstances(ctx context.Context, instanceIDs []string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	_, err := c.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: instanceIDs,
	})
	return err
}

// DescribeAllWorkerInstances returns all worker instances for the celeris-benchmarks project.
func (c *Client) DescribeAllWorkerInstances(ctx context.Context) ([]types.Instance, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   strPtr("tag:Project"),
				Values: []string{"celeris-benchmarks"},
			},
			{
				Name:   strPtr("instance-state-name"),
				Values: []string{"pending", "running"},
			},
		},
	}

	result, err := awsutil.WithRetry(ctx, "DescribeInstances", awsutil.DefaultMaxRetries, func() (*ec2.DescribeInstancesOutput, error) {
		return c.ec2.DescribeInstances(ctx, input)
	})
	if err != nil {
		return nil, err
	}

	var instances []types.Instance
	for _, res := range result.Reservations {
		instances = append(instances, res.Instances...)
	}

	return instances, nil
}

// fallbackPrices contains hardcoded on-demand prices for us-east-1 as fallback.
// These are used when the Pricing API is unavailable or returns no results.
var fallbackPrices = map[string]float64{
	// ARM64 - C6g family
	"c6g.medium":  0.034,
	"c6g.large":   0.068,
	"c6g.xlarge":  0.136,
	"c6g.2xlarge": 0.272,
	"c6g.4xlarge": 0.544,
	"c6g.8xlarge": 1.088,
	"c6g.metal":   2.176,
	// ARM64 - C7g family
	"c7g.medium":  0.0363,
	"c7g.large":   0.0725,
	"c7g.xlarge":  0.145,
	"c7g.2xlarge": 0.29,
	"c7g.4xlarge": 0.58,
	"c7g.metal":   2.32,
	// ARM64 - T4g family
	"t4g.micro":  0.0084,
	"t4g.small":  0.0168,
	"t4g.medium": 0.0336,
	// ARM64 - M7g family
	"m7g.metal": 3.2640,

	// x86 - C5 family
	"c5.large":   0.085,
	"c5.xlarge":  0.17,
	"c5.2xlarge": 0.34,
	"c5.9xlarge": 1.53,
	"c5.metal":   4.08,
	// x86 - C5a family
	"c5a.large":   0.077,
	"c5a.xlarge":  0.154,
	"c5a.2xlarge": 0.308,
	// x86 - C5d family
	"c5d.metal":   4.608,
	"c5d.9xlarge": 1.728,
	// x86 - C5n family
	"c5n.metal":   4.608,
	"c5n.9xlarge": 1.944,
	// x86 - C6i family
	"c6i.large":   0.085,
	"c6i.xlarge":  0.17,
	"c6i.2xlarge": 0.34,
	// x86 - T2/T3 family
	"t2.small":  0.023,
	"t3.small":  0.0208,
	"t3a.small": 0.0188,
}

// getOnDemandPrice retrieves the on-demand price for an instance type.
// It first checks the cache, then queries the AWS Pricing API, and falls back
// to hardcoded prices if the API is unavailable.
func (c *Client) getOnDemandPrice(ctx context.Context, instanceType string) (float64, error) {
	cacheKey := c.region + ":" + instanceType

	// Check cache first (with read lock)
	c.priceMu.RLock()
	if cached, ok := c.priceCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		c.priceMu.RUnlock()
		return cached.price, nil
	}
	c.priceMu.RUnlock()

	// Try to fetch from Pricing API
	price, err := c.fetchOnDemandPriceFromAPI(ctx, instanceType)
	if err != nil {
		slog.Warn("failed to fetch price from API, using fallback",
			"instance_type", instanceType,
			"region", c.region,
			"error", err)

		// Fall back to hardcoded prices
		if fallbackPrice, ok := fallbackPrices[instanceType]; ok {
			return fallbackPrice, nil
		}
		return 0, fmt.Errorf("unknown instance type: %s (no API price or fallback)", instanceType)
	}

	// Cache the price (with write lock)
	c.priceMu.Lock()
	c.priceCache[cacheKey] = &cachedPrice{
		price:     price,
		expiresAt: time.Now().Add(PriceCacheTTL),
	}
	c.priceMu.Unlock()

	slog.Debug("fetched on-demand price from API",
		"instance_type", instanceType,
		"region", c.region,
		"price", price)

	return price, nil
}

// fetchOnDemandPriceFromAPI queries the AWS Pricing API for on-demand EC2 prices.
func (c *Client) fetchOnDemandPriceFromAPI(ctx context.Context, instanceType string) (float64, error) {
	// Get location name for the region
	location, ok := regionLocations[c.region]
	if !ok {
		return 0, fmt.Errorf("unknown region: %s", c.region)
	}

	// Build filters for the Pricing API
	filters := []pricingtypes.Filter{
		{
			Type:  pricingtypes.FilterTypeTermMatch,
			Field: strPtr("instanceType"),
			Value: strPtr(instanceType),
		},
		{
			Type:  pricingtypes.FilterTypeTermMatch,
			Field: strPtr("location"),
			Value: strPtr(location),
		},
		{
			Type:  pricingtypes.FilterTypeTermMatch,
			Field: strPtr("operatingSystem"),
			Value: strPtr("Linux"),
		},
		{
			Type:  pricingtypes.FilterTypeTermMatch,
			Field: strPtr("tenancy"),
			Value: strPtr("Shared"),
		},
		{
			Type:  pricingtypes.FilterTypeTermMatch,
			Field: strPtr("preInstalledSw"),
			Value: strPtr("NA"),
		},
		{
			Type:  pricingtypes.FilterTypeTermMatch,
			Field: strPtr("capacitystatus"),
			Value: strPtr("Used"),
		},
	}

	input := &pricing.GetProductsInput{
		ServiceCode: strPtr("AmazonEC2"),
		Filters:     filters,
		MaxResults:  intPtr32(1),
	}

	result, err := awsutil.WithRetry(ctx, "GetProducts", awsutil.DefaultMaxRetries, func() (*pricing.GetProductsOutput, error) {
		return c.pricing.GetProducts(ctx, input)
	})
	if err != nil {
		return 0, fmt.Errorf("pricing API call failed: %w", err)
	}

	if len(result.PriceList) == 0 {
		return 0, fmt.Errorf("no pricing data found for %s in %s", instanceType, c.region)
	}

	// Parse the pricing JSON
	return parsePricingJSON(result.PriceList[0])
}

// parsePricingJSON extracts the on-demand hourly price from AWS Pricing API response.
func parsePricingJSON(priceJSON string) (float64, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(priceJSON), &data); err != nil {
		return 0, fmt.Errorf("failed to parse pricing JSON: %w", err)
	}

	// Navigate: terms -> OnDemand -> (first key) -> priceDimensions -> (first key) -> pricePerUnit -> USD
	terms, ok := data["terms"].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("missing 'terms' in pricing data")
	}

	onDemand, ok := terms["OnDemand"].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("missing 'OnDemand' in pricing data")
	}

	// Get first offer
	var offer map[string]any
	for _, v := range onDemand {
		offer, ok = v.(map[string]any)
		if ok {
			break
		}
	}
	if offer == nil {
		return 0, fmt.Errorf("no offers found in pricing data")
	}

	priceDimensions, ok := offer["priceDimensions"].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("missing 'priceDimensions' in pricing data")
	}

	// Get first price dimension
	var priceDim map[string]any
	for _, v := range priceDimensions {
		priceDim, ok = v.(map[string]any)
		if ok {
			break
		}
	}
	if priceDim == nil {
		return 0, fmt.Errorf("no price dimensions found")
	}

	pricePerUnit, ok := priceDim["pricePerUnit"].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("missing 'pricePerUnit' in pricing data")
	}

	usdPrice, ok := pricePerUnit["USD"].(string)
	if !ok {
		return 0, fmt.Errorf("missing 'USD' price in pricing data")
	}

	var price float64
	if _, err := fmt.Sscanf(usdPrice, "%f", &price); err != nil {
		return 0, fmt.Errorf("failed to parse price '%s': %w", usdPrice, err)
	}

	return price, nil
}

// intPtr32 returns a pointer to an int32 value.
func intPtr32(i int32) *int32 {
	return &i
}

// parsePrice converts a price string to float64.
func parsePrice(s string) float64 {
	var price float64
	_, _ = fmt.Sscanf(s, "%f", &price)
	return price
}

func strPtr(s string) *string {
	return &s
}

func intPtr(i int32) *int32 {
	return &i
}

func boolPtr(b bool) *bool {
	return &b
}
