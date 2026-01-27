output "arm64_spot_request_id" {
  description = "Spot instance request ID for ARM64 runner"
  value       = var.use_on_demand ? null : aws_spot_instance_request.benchmark_arm64[0].id
}

output "arm64_instance_id" {
  description = "Instance ID for ARM64 runner"
  value       = var.use_on_demand ? aws_instance.benchmark_arm64_ondemand[0].id : aws_spot_instance_request.benchmark_arm64[0].spot_instance_id
}

output "x86_spot_request_id" {
  description = "Spot instance request ID for x86 runner"
  value       = var.use_on_demand ? null : aws_spot_instance_request.benchmark_x86[0].id
}

output "x86_instance_id" {
  description = "Instance ID for x86 runner"
  value       = var.use_on_demand ? aws_instance.benchmark_x86_ondemand[0].id : aws_spot_instance_request.benchmark_x86[0].spot_instance_id
}

output "ami_arm64" {
  description = "AMI used for ARM64 instance"
  value       = data.aws_ami.ubuntu_arm64.id
}

output "ami_x86" {
  description = "AMI used for x86 instance"
  value       = data.aws_ami.ubuntu_x86.id
}

output "runner_version" {
  description = "GitHub Actions runner version"
  value       = local.runner_version
}

output "instance_mode" {
  description = "Whether using spot or on-demand instances"
  value       = var.use_on_demand ? "on-demand" : "spot"
}
