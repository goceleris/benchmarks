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

// Instance types for each mode and architecture
var InstanceTypes = map[string]map[string]struct{ Server, Client string }{
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
	bidPrice := bestPrice.Price * 1.20
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

// GetBestAZForPair finds the best AZ for a server+client pair.
// Both must be in the same AZ, so we pick the cheapest for the larger instance.
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

	serverBid := bestServerPrice * 1.20
	if serverOnDemand > 0 && serverBid > serverOnDemand {
		serverBid = serverOnDemand
	}

	clientBid := bestClientPrice * 1.20
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

// GetInstancesForMode returns the instance types for a benchmark mode and architecture.
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

// CheckSpotInterruption checks if any instances have spot interruption notices.
func (c *Client) CheckSpotInterruption(ctx context.Context, instanceIDs []string) ([]string, error) {
	// This would require IMDSv2 access from each instance
	// For now, we rely on the instances self-reporting via the C2 API
	return nil, nil
}

// getOnDemandPrice retrieves the on-demand price for an instance type.
func (c *Client) getOnDemandPrice(ctx context.Context, instanceType string) (float64, error) {
	// On-demand pricing is complex to query; use hardcoded approximations for now
	// TODO: Implement proper pricing API query

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
