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
  description = "Benchmark mode: 'fast' for quick PR validation (c5.large/c6g.medium), 'metal' for official results (c5.metal/c6g.metal)"
  type        = string
  default     = "fast"

  validation {
    condition     = contains(["fast", "metal"], var.benchmark_mode)
    error_message = "benchmark_mode must be 'fast' or 'metal'"
  }
}

# Instance type mappings
locals {
  # Fast mode: cheaper virtualized instances (same CPU family as metal)
  # Metal mode: bare metal for official results
  instance_types = {
    fast = {
      arm64 = "c6g.medium"  # 1 vCPU, virtualized slice of c6g.metal
      x86   = "c5.large"    # 2 vCPU, virtualized slice of c5.metal
    }
    metal = {
      arm64 = "c6g.metal"   # Full Graviton2, bare metal
      x86   = "c5.metal"    # Full Intel, bare metal
    }
  }

  # Spot prices per mode
  spot_prices = {
    fast = {
      arm64 = "0.10"   # ~$0.034/hr on-demand, $0.10 max spot
      x86   = "0.20"   # ~$0.085/hr on-demand, $0.20 max spot
    }
    metal = {
      arm64 = "2.50"   # ~$2.176/hr on-demand
      x86   = "4.00"   # ~$4.08/hr on-demand
    }
  }

  # Runner labels per mode (so workflows can target correct runners)
  runner_labels = {
    fast = {
      arm64 = ["self-hosted", "fast-arm64", "linux", "arm64"]
      x86   = ["self-hosted", "fast-x86", "linux", "x86_64"]
    }
    metal = {
      arm64 = ["self-hosted", "metal-arm64", "linux", "arm64"]
      x86   = ["self-hosted", "metal-x86", "linux", "x86_64"]
    }
  }

  # Wait time for runners (metal instances take longer to boot)
  runner_wait_time = var.benchmark_mode == "metal" ? 120 : 60
}

# Selected configuration based on mode
output "selected_instance_arm64" {
  description = "Selected ARM64 instance type"
  value       = local.instance_types[var.benchmark_mode].arm64
}

output "selected_instance_x86" {
  description = "Selected x86 instance type"
  value       = local.instance_types[var.benchmark_mode].x86
}

output "benchmark_mode" {
  description = "Current benchmark mode"
  value       = var.benchmark_mode
}
