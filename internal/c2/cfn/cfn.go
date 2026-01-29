// Package cfn provides CloudFormation operations for managing worker stacks.
package cfn

import (
	"context"
	"embed"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

//go:embed templates/*.yaml
var templates embed.FS

// Client wraps AWS CloudFormation client.
type Client struct {
	cfn    *cloudformation.Client
	region string
}

// WorkerStackParams contains parameters for creating a worker stack.
type WorkerStackParams struct {
	RunID             string
	Mode              string // fast, med, metal
	Architecture      string // arm64, x86
	AvailabilityZone  string
	SpotPrice         string
	SubnetID          string
	SecurityGroupID   string
	InstanceProfileArn string
	C2Endpoint        string
	BenchmarkDuration string
	BenchmarkMode     string // baseline, theoretical, all
}

// StackStatus represents the status of a CloudFormation stack.
type StackStatus struct {
	Name   string
	Status string
	Reason string
}

// NewClient creates a new CloudFormation client.
func NewClient(region string) (*Client, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &Client{
		cfn:    cloudformation.NewFromConfig(cfg),
		region: region,
	}, nil
}

// CreateWorkerStack creates a CloudFormation stack for workers.
func (c *Client) CreateWorkerStack(ctx context.Context, params WorkerStackParams) (string, error) {
	stackName := fmt.Sprintf("celeris-workers-%s-%s", params.RunID, params.Architecture)

	// Read template from embedded files
	templateBody, err := templates.ReadFile("templates/workers.yaml")
	if err != nil {
		return "", fmt.Errorf("failed to read worker template: %w", err)
	}

	input := &cloudformation.CreateStackInput{
		StackName:    &stackName,
		TemplateBody: strPtr(string(templateBody)),
		Parameters: []types.Parameter{
			{ParameterKey: strPtr("RunId"), ParameterValue: &params.RunID},
			{ParameterKey: strPtr("Mode"), ParameterValue: &params.Mode},
			{ParameterKey: strPtr("Architecture"), ParameterValue: &params.Architecture},
			{ParameterKey: strPtr("AvailabilityZone"), ParameterValue: &params.AvailabilityZone},
			{ParameterKey: strPtr("SpotPrice"), ParameterValue: &params.SpotPrice},
			{ParameterKey: strPtr("SubnetId"), ParameterValue: &params.SubnetID},
			{ParameterKey: strPtr("SecurityGroupId"), ParameterValue: &params.SecurityGroupID},
			{ParameterKey: strPtr("InstanceProfileArn"), ParameterValue: &params.InstanceProfileArn},
			{ParameterKey: strPtr("C2Endpoint"), ParameterValue: &params.C2Endpoint},
			{ParameterKey: strPtr("BenchmarkDuration"), ParameterValue: &params.BenchmarkDuration},
			{ParameterKey: strPtr("BenchmarkMode"), ParameterValue: &params.BenchmarkMode},
		},
		Capabilities: []types.Capability{
			types.CapabilityCapabilityIam,
		},
		Tags: []types.Tag{
			{Key: strPtr("Project"), Value: strPtr("celeris-benchmarks")},
			{Key: strPtr("RunId"), Value: &params.RunID},
			{Key: strPtr("Architecture"), Value: &params.Architecture},
		},
		OnFailure: types.OnFailureDelete,
	}

	result, err := c.cfn.CreateStack(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to create stack: %w", err)
	}

	log.Printf("Created stack %s: %s", stackName, *result.StackId)
	return stackName, nil
}

// WaitForStack waits for a stack to reach a terminal state.
func (c *Client) WaitForStack(ctx context.Context, stackName string, timeout time.Duration) (*StackStatus, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		status, err := c.GetStackStatus(ctx, stackName)
		if err != nil {
			return nil, err
		}

		switch status.Status {
		case "CREATE_COMPLETE", "UPDATE_COMPLETE":
			return status, nil
		case "CREATE_FAILED", "ROLLBACK_COMPLETE", "ROLLBACK_FAILED", "DELETE_COMPLETE", "DELETE_FAILED":
			return status, fmt.Errorf("stack %s failed: %s - %s", stackName, status.Status, status.Reason)
		}

		time.Sleep(10 * time.Second)
	}

	return nil, fmt.Errorf("timeout waiting for stack %s", stackName)
}

// GetStackStatus returns the current status of a stack.
func (c *Client) GetStackStatus(ctx context.Context, stackName string) (*StackStatus, error) {
	result, err := c.cfn.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
		StackName: &stackName,
	})
	if err != nil {
		return nil, err
	}

	if len(result.Stacks) == 0 {
		return nil, fmt.Errorf("stack not found: %s", stackName)
	}

	stack := result.Stacks[0]
	status := &StackStatus{
		Name:   stackName,
		Status: string(stack.StackStatus),
	}
	if stack.StackStatusReason != nil {
		status.Reason = *stack.StackStatusReason
	}

	return status, nil
}

// DeleteStack deletes a CloudFormation stack.
func (c *Client) DeleteStack(ctx context.Context, stackName string) error {
	_, err := c.cfn.DeleteStack(ctx, &cloudformation.DeleteStackInput{
		StackName: &stackName,
	})
	if err != nil {
		return fmt.Errorf("failed to delete stack %s: %w", stackName, err)
	}

	log.Printf("Deleting stack %s", stackName)
	return nil
}

// DeleteWorkerStacks deletes all worker stacks for a run.
func (c *Client) DeleteWorkerStacks(ctx context.Context, runID string) error {
	// List stacks with the run ID
	result, err := c.cfn.ListStacks(ctx, &cloudformation.ListStacksInput{
		StackStatusFilter: []types.StackStatus{
			types.StackStatusCreateComplete,
			types.StackStatusUpdateComplete,
			types.StackStatusCreateInProgress,
			types.StackStatusUpdateInProgress,
		},
	})
	if err != nil {
		return err
	}

	prefix := fmt.Sprintf("celeris-workers-%s-", runID)
	for _, stack := range result.StackSummaries {
		if stack.StackName != nil && len(*stack.StackName) > len(prefix) && (*stack.StackName)[:len(prefix)] == prefix {
			if err := c.DeleteStack(ctx, *stack.StackName); err != nil {
				log.Printf("Warning: Failed to delete stack %s: %v", *stack.StackName, err)
			}
		}
	}

	return nil
}

// GetStackOutputs returns the outputs of a stack.
func (c *Client) GetStackOutputs(ctx context.Context, stackName string) (map[string]string, error) {
	result, err := c.cfn.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
		StackName: &stackName,
	})
	if err != nil {
		return nil, err
	}

	if len(result.Stacks) == 0 {
		return nil, fmt.Errorf("stack not found: %s", stackName)
	}

	outputs := make(map[string]string)
	for _, output := range result.Stacks[0].Outputs {
		if output.OutputKey != nil && output.OutputValue != nil {
			outputs[*output.OutputKey] = *output.OutputValue
		}
	}

	return outputs, nil
}

func strPtr(s string) *string {
	return &s
}
