// Package config handles loading configuration from SSM Parameter Store and environment.
package config

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Config holds all C2 configuration.
type Config struct {
	// AWS
	Region string

	// Secrets
	GitHubToken string
	APIKey      string

	// Infrastructure (from C2 stack outputs)
	SubnetID           string
	SubnetsByAZ        map[string]string // AZ -> SubnetID mapping for all public subnets
	SecurityGroupID    string
	InstanceProfileArn string
	C2Endpoint         string
	VpcID              string
	RouteTableID       string // Route table for creating new subnets
}

// Loader loads configuration from various sources.
type Loader struct {
	ssm *ssm.Client
	cfn *cloudformation.Client
	ec2 *ec2.Client
}

// NewLoader creates a new configuration loader.
func NewLoader(region string) (*Loader, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &Loader{
		ssm: ssm.NewFromConfig(cfg),
		cfn: cloudformation.NewFromConfig(cfg),
		ec2: ec2.NewFromConfig(cfg),
	}, nil
}

// Load loads all configuration.
func (l *Loader) Load(ctx context.Context) (*Config, error) {
	cfg := &Config{}

	// Load from environment first (allows overrides)
	cfg.Region = getEnvOrDefault("AWS_REGION", "us-east-1")
	cfg.GitHubToken = os.Getenv("GITHUB_TOKEN")
	cfg.APIKey = os.Getenv("C2_API_KEY")
	cfg.SubnetID = os.Getenv("SUBNET_ID")
	cfg.SecurityGroupID = os.Getenv("SECURITY_GROUP_ID")
	cfg.InstanceProfileArn = os.Getenv("INSTANCE_PROFILE_ARN")
	cfg.C2Endpoint = os.Getenv("C2_ENDPOINT")

	// Load secrets from SSM if not in environment
	if cfg.GitHubToken == "" {
		token, err := l.getSSMParameter(ctx, "/celeris/github-token")
		if err != nil {
			log.Printf("Warning: Could not load GitHub token from SSM: %v", err)
		} else {
			cfg.GitHubToken = token
		}
	}

	if cfg.APIKey == "" {
		key, err := l.getSSMParameter(ctx, "/celeris/api-key")
		if err != nil {
			log.Printf("Warning: Could not load API key from SSM: %v", err)
		} else {
			cfg.APIKey = key
		}
	}

	// Load infrastructure config from CloudFormation if not in environment
	if cfg.SubnetID == "" || cfg.SecurityGroupID == "" || cfg.InstanceProfileArn == "" {
		if err := l.loadFromCloudFormation(ctx, cfg); err != nil {
			log.Printf("Warning: Could not load config from CloudFormation: %v", err)
		}
	}

	// Get VPC ID from security group if not already set
	if cfg.VpcID == "" && cfg.SecurityGroupID != "" {
		sgResult, err := l.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			GroupIds: []string{cfg.SecurityGroupID},
		})
		if err == nil && len(sgResult.SecurityGroups) > 0 && sgResult.SecurityGroups[0].VpcId != nil {
			cfg.VpcID = *sgResult.SecurityGroups[0].VpcId
			log.Printf("Discovered VPC ID from security group: %s", cfg.VpcID)
		}
	}

	// Discover C2 endpoint if not set
	if cfg.C2Endpoint == "" {
		endpoint, err := l.discoverC2Endpoint(ctx)
		if err != nil {
			log.Printf("Warning: Could not discover C2 endpoint: %v", err)
		} else {
			cfg.C2Endpoint = endpoint
		}
	}

	// Find a suitable subnet if still not set
	if cfg.SubnetID == "" && cfg.VpcID != "" {
		subnet, err := l.findPublicSubnet(ctx, cfg.VpcID)
		if err != nil {
			log.Printf("Warning: Could not find public subnet: %v", err)
		} else {
			cfg.SubnetID = subnet
		}
	}

	// Discover all public subnets in the VPC mapped by AZ
	if cfg.VpcID != "" && cfg.SubnetsByAZ == nil {
		subnets, err := l.discoverPublicSubnets(ctx, cfg.VpcID)
		if err != nil {
			log.Printf("Warning: Could not discover public subnets: %v", err)
		} else {
			cfg.SubnetsByAZ = subnets
			log.Printf("Discovered %d public subnets: %v", len(subnets), subnets)
		}
	}

	// Discover route table for creating new subnets
	if cfg.VpcID != "" && cfg.RouteTableID == "" {
		rtID, err := l.discoverPublicRouteTable(ctx, cfg.VpcID)
		if err != nil {
			log.Printf("Warning: Could not discover route table: %v", err)
		} else {
			cfg.RouteTableID = rtID
			log.Printf("Discovered public route table: %s", rtID)
		}
	}

	return cfg, nil
}

// getSSMParameter retrieves a parameter from SSM Parameter Store.
func (l *Loader) getSSMParameter(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	result, err := l.ssm.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           &name,
		WithDecryption: boolPtr(true),
	})
	if err != nil {
		return "", err
	}

	if result.Parameter == nil || result.Parameter.Value == nil {
		return "", fmt.Errorf("parameter %s has no value", name)
	}

	return *result.Parameter.Value, nil
}

// loadFromCloudFormation loads infrastructure config from the C2 stack outputs.
func (l *Loader) loadFromCloudFormation(ctx context.Context, cfg *Config) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	stackName := "celeris-c2"
	result, err := l.cfn.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
		StackName: &stackName,
	})
	if err != nil {
		return fmt.Errorf("failed to describe stack: %w", err)
	}

	if len(result.Stacks) == 0 {
		return fmt.Errorf("stack %s not found", stackName)
	}

	stack := result.Stacks[0]
	for _, output := range stack.Outputs {
		if output.OutputKey == nil || output.OutputValue == nil {
			continue
		}

		switch *output.OutputKey {
		case "WorkerSecurityGroupId":
			if cfg.SecurityGroupID == "" {
				cfg.SecurityGroupID = *output.OutputValue
			}
		case "WorkerInstanceProfileArn":
			if cfg.InstanceProfileArn == "" {
				cfg.InstanceProfileArn = *output.OutputValue
			}
		case "C2ElasticIP":
			if cfg.C2Endpoint == "" {
				cfg.C2Endpoint = fmt.Sprintf("http://%s:8080", *output.OutputValue)
			}
		}
	}

	// Get VPC ID from security group
	if cfg.SecurityGroupID != "" && cfg.VpcID == "" {
		sgResult, err := l.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			GroupIds: []string{cfg.SecurityGroupID},
		})
		if err == nil && len(sgResult.SecurityGroups) > 0 && sgResult.SecurityGroups[0].VpcId != nil {
			cfg.VpcID = *sgResult.SecurityGroups[0].VpcId
		}
	}

	return nil
}

// discoverC2Endpoint discovers the C2 endpoint from instance metadata or EIP.
func (l *Loader) discoverC2Endpoint(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Try to find the C2 instance's public IP
	result, err := l.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   strPtr("tag:Name"),
				Values: []string{"celeris-c2"},
			},
			{
				Name:   strPtr("instance-state-name"),
				Values: []string{"running"},
			},
		},
	})
	if err != nil {
		return "", err
	}

	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			if instance.PublicIpAddress != nil {
				return fmt.Sprintf("http://%s:8080", *instance.PublicIpAddress), nil
			}
		}
	}

	return "", fmt.Errorf("C2 instance not found")
}

// discoverPublicSubnets returns all public subnets in the VPC mapped by AZ.
func (l *Loader) discoverPublicSubnets(ctx context.Context, vpcID string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	result, err := l.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{
			{
				Name:   strPtr("vpc-id"),
				Values: []string{vpcID},
			},
			{
				Name:   strPtr("map-public-ip-on-launch"),
				Values: []string{"true"},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	subnets := make(map[string]string)
	for _, subnet := range result.Subnets {
		if subnet.AvailabilityZone != nil && subnet.SubnetId != nil {
			subnets[*subnet.AvailabilityZone] = *subnet.SubnetId
		}
	}

	return subnets, nil
}

// discoverPublicRouteTable returns the public route table (one with an IGW route) for the VPC.
func (l *Loader) discoverPublicRouteTable(ctx context.Context, vpcID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	result, err := l.ec2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{
			{
				Name:   strPtr("vpc-id"),
				Values: []string{vpcID},
			},
		},
	})
	if err != nil {
		return "", err
	}

	// Find a route table with an internet gateway route
	for _, rt := range result.RouteTables {
		for _, route := range rt.Routes {
			if route.GatewayId != nil && len(*route.GatewayId) > 3 && (*route.GatewayId)[:4] == "igw-" {
				if rt.RouteTableId != nil {
					return *rt.RouteTableId, nil
				}
			}
		}
	}

	return "", fmt.Errorf("no public route table found in VPC %s", vpcID)
}

// findPublicSubnet finds a public subnet in the VPC.
func (l *Loader) findPublicSubnet(ctx context.Context, vpcID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	result, err := l.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{
			{
				Name:   strPtr("vpc-id"),
				Values: []string{vpcID},
			},
			{
				Name:   strPtr("map-public-ip-on-launch"),
				Values: []string{"true"},
			},
		},
	})
	if err != nil {
		return "", err
	}

	if len(result.Subnets) == 0 {
		return "", fmt.Errorf("no public subnets found in VPC %s", vpcID)
	}

	// Return the first one
	if result.Subnets[0].SubnetId != nil {
		return *result.Subnets[0].SubnetId, nil
	}

	return "", fmt.Errorf("subnet has no ID")
}

func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

func strPtr(s string) *string {
	return &s
}

func boolPtr(b bool) *bool {
	return &b
}
