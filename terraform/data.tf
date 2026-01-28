# Fetch latest Ubuntu 22.04 LTS AMI for ARM64 (Graviton)
data "aws_ami" "ubuntu_arm64" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-arm64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }

  filter {
    name   = "architecture"
    values = ["arm64"]
  }
}

# Fetch latest Ubuntu 22.04 LTS AMI for x86_64
data "aws_ami" "ubuntu_x86" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }

  filter {
    name   = "architecture"
    values = ["x86_64"]
  }
}

# Get current AWS account ID for IAM policy
data "aws_caller_identity" "current" {}

# Get current region
data "aws_region" "current" {}

# Get available availability zones
data "aws_availability_zones" "available" {
  state = "available"
}

# GitHub Actions runner version
# Hardcoded to avoid GitHub API rate limit issues on shared runner IPs
# Update periodically from: https://github.com/actions/runner/releases
# Note: v2.327.1+ required for node24 support (used by actions/checkout@v6)
locals {
  runner_version = "v2.331.0"
  # Use first available AZ to ensure client and server are in the same AZ
  # This reduces cross-AZ latency (~0.5-1ms) that adds noise to benchmarks
  availability_zone = data.aws_availability_zones.available.names[0]
}
