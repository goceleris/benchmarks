output "arm64_spot_request_id" {
  description = "Spot instance request ID for ARM64 runner"
  value       = length(aws_spot_instance_request.arm64_runner) > 0 ? aws_spot_instance_request.arm64_runner[0].id : null
}

output "arm64_instance_id" {
  description = "Instance ID for ARM64 runner"
  value = (
    length(aws_instance.arm64_runner_ondemand) > 0 ? aws_instance.arm64_runner_ondemand[0].id :
    length(aws_spot_instance_request.arm64_runner) > 0 ? aws_spot_instance_request.arm64_runner[0].spot_instance_id :
    null
  )
}

output "x86_spot_request_id" {
  description = "Spot instance request ID for x86 runner"
  value       = length(aws_spot_instance_request.x86_runner) > 0 ? aws_spot_instance_request.x86_runner[0].id : null
}

output "x86_instance_id" {
  description = "Instance ID for x86 runner"
  value = (
    length(aws_instance.x86_runner_ondemand) > 0 ? aws_instance.x86_runner_ondemand[0].id :
    length(aws_spot_instance_request.x86_runner) > 0 ? aws_spot_instance_request.x86_runner[0].spot_instance_id :
    null
  )
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

output "launch_mode" {
  description = "Which architectures are being launched"
  value = (
    var.launch_arm64_only ? "arm64-only" :
    var.launch_x86_only ? "x86-only" :
    "both"
  )
}
