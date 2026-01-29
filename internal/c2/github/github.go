// Package github provides GitHub API operations for PR creation.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/goceleris/benchmarks/internal/c2/store"
)

const (
	githubAPI = "https://api.github.com"
	owner     = "goceleris"
	repo      = "benchmarks"
)

// Client wraps GitHub API operations.
type Client struct {
	token      string
	httpClient *http.Client
}

// NewClient creates a new GitHub client from environment variable.
func NewClient() (*Client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN not set")
	}
	return NewClientWithToken(token)
}

// NewClientWithToken creates a new GitHub client with the given token.
func NewClientWithToken(token string) (*Client, error) {
	if token == "" {
		return nil, fmt.Errorf("token is required")
	}

	return &Client{
		token: token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// CreateResultsPR creates a PR with benchmark results.
func (c *Client) CreateResultsPR(ctx context.Context, run *store.Run) error {
	log.Printf("Creating results PR for run %s", run.ID)

	// Create branch
	branchName := fmt.Sprintf("benchmark-results-%s", run.ID)
	baseSHA, err := c.getDefaultBranchSHA(ctx)
	if err != nil {
		return fmt.Errorf("failed to get base SHA: %w", err)
	}

	if err := c.createBranch(ctx, branchName, baseSHA); err != nil {
		return fmt.Errorf("failed to create branch: %w", err)
	}

	// Commit results for each architecture
	for arch, result := range run.Results {
		if result.Status != "completed" {
			continue
		}

		archResults, _ := json.MarshalIndent(result, "", "  ")
		filePath := fmt.Sprintf("results/latest/%s/benchmark.json", arch)

		if err := c.createOrUpdateFile(ctx, branchName, filePath, archResults,
			fmt.Sprintf("Update %s benchmark results [skip ci]", arch)); err != nil {
			log.Printf("Warning: Failed to commit %s results: %v", arch, err)
		}
	}

	// Build PR body with results summary
	prBody := fmt.Sprintf(`## Benchmark Results

Run ID: %s
Mode: %s
Duration: %s

### Results Summary

| Architecture | Status | Benchmarks | Top RPS |
|--------------|--------|------------|---------|
`, run.ID, run.Mode, run.Duration)

	for arch, result := range run.Results {
		topRPS := 0.0
		for _, bench := range result.Benchmarks {
			if bench.RequestsPerSec > topRPS {
				topRPS = bench.RequestsPerSec
			}
		}
		prBody += fmt.Sprintf("| %s | %s | %d | %.0f |\n",
			arch, result.Status, len(result.Benchmarks), topRPS)
	}

	// Create PR
	prURL, err := c.createPR(ctx, branchName,
		fmt.Sprintf("chore: update benchmark results (%s) [skip ci]", run.Mode),
		prBody)

	if err != nil {
		return fmt.Errorf("failed to create PR: %w", err)
	}

	log.Printf("Created PR: %s", prURL)
	return nil
}

// getDefaultBranchSHA returns the SHA of the default branch HEAD.
func (c *Client) getDefaultBranchSHA(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/git/ref/heads/main", githubAPI, owner, repo)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to get ref: %s - %s", resp.Status, string(body))
	}

	var result struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.Object.SHA, nil
}

// createBranch creates a new branch.
func (c *Client) createBranch(ctx context.Context, name, sha string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/git/refs", githubAPI, owner, repo)

	body, _ := json.Marshal(map[string]string{
		"ref": "refs/heads/" + name,
		"sha": sha,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create branch: %s - %s", resp.Status, string(respBody))
	}

	return nil
}

// createOrUpdateFile creates or updates a file in the repository.
func (c *Client) createOrUpdateFile(ctx context.Context, branch, path string, content []byte, message string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", githubAPI, owner, repo, path)

	// Check if file exists to get SHA
	var existingSHA string
	req, _ := http.NewRequestWithContext(ctx, "GET", url+"?ref="+branch, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err == nil && resp.StatusCode == http.StatusOK {
		var existing struct {
			SHA string `json:"sha"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&existing)
		existingSHA = existing.SHA
		_ = resp.Body.Close()
	}

	// Create/update file
	bodyData := map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
	}
	if existingSHA != "" {
		bodyData["sha"] = existingSHA
	}

	body, _ := json.Marshal(bodyData)

	req, err = http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err = c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create file: %s - %s", resp.Status, string(respBody))
	}

	return nil
}

// createPR creates a pull request.
func (c *Client) createPR(ctx context.Context, branch, title, body string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls", githubAPI, owner, repo)

	reqBody, _ := json.Marshal(map[string]string{
		"title": title,
		"head":  branch,
		"base":  "main",
		"body":  body,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to create PR: %s - %s", resp.Status, string(respBody))
	}

	var result struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.HTMLURL, nil
}
