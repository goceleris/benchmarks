output "arm64_spot_request_id" {
  description = "Spot instance request ID for ARM64 runner"
  value       = aws_spot_instance_request.benchmark_arm64.id
}

output "arm64_instance_id" {
  description = "Instance ID for ARM64 runner (available after fulfillment)"
  value       = aws_spot_instance_request.benchmark_arm64.spot_instance_id
}

output "x86_spot_request_id" {
  description = "Spot instance request ID for x86 runner"
  value       = aws_spot_instance_request.benchmark_x86.id
}

output "x86_instance_id" {
  description = "Instance ID for x86 runner (available after fulfillment)"
  value       = aws_spot_instance_request.benchmark_x86.spot_instance_id
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
