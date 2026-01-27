variable "aws_region" {
  description = "AWS region for benchmark infrastructure"
  type        = string
  default     = "us-east-1"
}

variable "gh_pat_runner_token" {
  description = "GitHub PAT with Administration: Read & Write permissions for runner registration"
  type        = string
  sensitive   = true
}

variable "repository_url" {
  description = "Full GitHub repository URL (e.g., https://github.com/owner/repo)"
  type        = string
  default     = "https://github.com/goceleris/benchmarks"
}

variable "benchmark_mode" {
  description = "Benchmark mode: 'fast' for quick PR validation, 'metal' for official results, 'provisional' for best-effort when metal unavailable"
  type        = string
  default     = "fast"

  validation {
    condition     = contains(["fast", "metal", "provisional"], var.benchmark_mode)
    error_message = "benchmark_mode must be 'fast', 'metal', or 'provisional'"
  }
}

variable "use_on_demand" {
  description = "Use on-demand instances instead of spot (fallback when spot quota/capacity unavailable)"
  type        = bool
  default     = false
}

variable "use_provisional" {
  description = "Use provisional instances (best available within quota limits) when metal unavailable"
  type        = bool
  default     = false
}

variable "launch_arm64_only" {
  description = "Only launch ARM64 infrastructure (for independent architecture launches)"
  type        = bool
  default     = false
}

variable "launch_x86_only" {
  description = "Only launch x86 infrastructure (for independent architecture launches)"
  type        = bool
  default     = false
}

# Pre-existing infrastructure references
# These must be created manually before running Terraform
variable "iam_instance_profile_name" {
  description = "Name of pre-existing IAM instance profile for benchmark runners"
  type        = string
  default     = "benchmark_runner_profile"
}

variable "security_group_id" {
  description = "ID of pre-existing security group for benchmark runners"
  type        = string
  default     = ""  # If empty, instances will use default VPC security
}

variable "subnet_id" {
  description = "Subnet ID for instances (optional, uses default if empty)"
  type        = string
  default     = ""
}

# Instance type mappings
locals {
  # Fast mode: cheaper virtualized instances for PR validation
  # Metal mode: bare metal for official results
  # Provisional mode: best available within quota limits (8 vCPUs) when metal unavailable
  instance_types = {
    fast = {
      arm64 = "c6g.medium"  # 1 vCPU
      x86   = "c5.large"    # 2 vCPU
    }
    metal = {
      arm64 = "c6g.metal"   # 64 vCPU, bare metal
      x86   = "c5.metal"    # 96 vCPU, bare metal
    }
    provisional = {
      arm64 = "c6g.2xlarge" # 8 vCPU, best within typical quota
      x86   = "c5.2xlarge"  # 8 vCPU, best within typical quota
    }
  }

  # Spot prices per mode
  spot_prices = {
    fast = {
      arm64 = "0.10"
      x86   = "0.20"
    }
    metal = {
      arm64 = "2.50"
      x86   = "4.00"
    }
    provisional = {
      arm64 = "0.40"
      x86   = "0.50"
    }
  }

  # Runner labels per mode
  runner_labels = {
    fast = {
      arm64 = ["self-hosted", "fast-arm64", "linux", "arm64"]
      x86   = ["self-hosted", "fast-x86", "linux", "x86_64"]
    }
    metal = {
      arm64 = ["self-hosted", "metal-arm64", "linux", "arm64"]
      x86   = ["self-hosted", "metal-x86", "linux", "x86_64"]
    }
    provisional = {
      arm64 = ["self-hosted", "provisional-arm64", "linux", "arm64"]
      x86   = ["self-hosted", "provisional-x86", "linux", "x86_64"]
    }
  }

  # Effective mode: provisional overrides metal when use_provisional is true
  effective_mode = var.use_provisional ? "provisional" : var.benchmark_mode
}

# Selected configuration based on mode
output "selected_instance_arm64" {
  description = "Selected ARM64 instance type"
  value       = local.instance_types[local.effective_mode].arm64
}

output "selected_instance_x86" {
  description = "Selected x86 instance type"
  value       = local.instance_types[local.effective_mode].x86
}

output "benchmark_mode" {
  description = "Current benchmark mode"
  value       = var.benchmark_mode
}

output "effective_mode" {
  description = "Effective mode (provisional if use_provisional is true)"
  value       = local.effective_mode
}
