// Package spot provides AWS spot instance management for the C2 server.
// It handles spot pricing, AZ selection, and instance lifecycle.
package spot

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
)

// Client wraps AWS EC2 client with spot-specific operations.
type Client struct {
	ec2     *ec2.Client
	pricing *pricing.Client
	region  string
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
		ec2:     ec2.NewFromConfig(cfg),
		pricing: pricing.NewFromConfig(pricingCfg),
		region:  region,
	}, nil
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

	result, err := c.ec2.DescribeSpotPriceHistory(ctx, input)
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

	// Build bid results - use 50% above current spot for better fulfillment
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

	// First, get AZ ID to name mapping
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

	result, err := c.ec2.GetSpotPlacementScores(ctx, input)
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

	result, err := c.ec2.DescribeInstances(ctx, input)
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

	result, err := c.ec2.DescribeInstances(ctx, input)
	if err != nil {
		return nil, err
	}

	var instances []types.Instance
	for _, res := range result.Reservations {
		instances = append(instances, res.Instances...)
	}

	return instances, nil
}

// getOnDemandPrice retrieves the on-demand price for an instance type.
// Uses hardcoded prices for us-east-1 region (prices are region-specific).
func (c *Client) getOnDemandPrice(ctx context.Context, instanceType string) (float64, error) {
	// Hardcoded on-demand prices for us-east-1 (updated periodically)
	// These serve as caps for spot bids
	prices := map[string]float64{
		// ARM64
		"c6g.medium":  0.034,
		"c6g.xlarge":  0.136,
		"c6g.2xlarge": 0.272,
		"c6g.4xlarge": 0.544,
		"c6g.metal":   2.176,
		"t4g.small":   0.0168,

		// x86
		"c5.large":   0.085,
		"c5.xlarge":  0.17,
		"c5.2xlarge": 0.34,
		"c5.9xlarge": 1.53,
		"c5.metal":   4.08,
		"t3.small":   0.0208,
	}

	if price, ok := prices[instanceType]; ok {
		return price, nil
	}

	return 0, fmt.Errorf("unknown instance type: %s", instanceType)
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
