# =============================================================================
# SERVER OUTPUTS
# =============================================================================

output "arm64_server_spot_request_id" {
  description = "Spot instance request ID for ARM64 server"
  value       = length(aws_spot_instance_request.arm64_server) > 0 ? aws_spot_instance_request.arm64_server[0].id : null
}

output "arm64_server_instance_id" {
  description = "Instance ID for ARM64 server"
  value = (
    length(aws_instance.arm64_server_ondemand) > 0 ? aws_instance.arm64_server_ondemand[0].id :
    length(aws_spot_instance_request.arm64_server) > 0 ? aws_spot_instance_request.arm64_server[0].spot_instance_id :
    null
  )
}

output "arm64_server_private_ip" {
  description = "Private IP of ARM64 server (for client to connect)"
  value = (
    length(aws_instance.arm64_server_ondemand) > 0 ? aws_instance.arm64_server_ondemand[0].private_ip :
    length(aws_spot_instance_request.arm64_server) > 0 ? aws_spot_instance_request.arm64_server[0].private_ip :
    null
  )
}

output "x86_server_spot_request_id" {
  description = "Spot instance request ID for x86 server"
  value       = length(aws_spot_instance_request.x86_server) > 0 ? aws_spot_instance_request.x86_server[0].id : null
}

output "x86_server_instance_id" {
  description = "Instance ID for x86 server"
  value = (
    length(aws_instance.x86_server_ondemand) > 0 ? aws_instance.x86_server_ondemand[0].id :
    length(aws_spot_instance_request.x86_server) > 0 ? aws_spot_instance_request.x86_server[0].spot_instance_id :
    null
  )
}

output "x86_server_private_ip" {
  description = "Private IP of x86 server (for client to connect)"
  value = (
    length(aws_instance.x86_server_ondemand) > 0 ? aws_instance.x86_server_ondemand[0].private_ip :
    length(aws_spot_instance_request.x86_server) > 0 ? aws_spot_instance_request.x86_server[0].private_ip :
    null
  )
}

# =============================================================================
# CLIENT OUTPUTS
# =============================================================================

output "arm64_client_instance_id" {
  description = "Instance ID for ARM64 client"
  value       = length(aws_instance.arm64_client) > 0 ? aws_instance.arm64_client[0].id : null
}

output "x86_client_instance_id" {
  description = "Instance ID for x86 client"
  value       = length(aws_instance.x86_client) > 0 ? aws_instance.x86_client[0].id : null
}

# =============================================================================
# BACKWARDS COMPATIBILITY OUTPUTS
# =============================================================================

output "arm64_spot_request_id" {
  description = "DEPRECATED: Use arm64_server_spot_request_id"
  value       = length(aws_spot_instance_request.arm64_server) > 0 ? aws_spot_instance_request.arm64_server[0].id : null
}

output "arm64_instance_id" {
  description = "DEPRECATED: Use arm64_server_instance_id"
  value = (
    length(aws_instance.arm64_server_ondemand) > 0 ? aws_instance.arm64_server_ondemand[0].id :
    length(aws_spot_instance_request.arm64_server) > 0 ? aws_spot_instance_request.arm64_server[0].spot_instance_id :
    null
  )
}

output "x86_spot_request_id" {
  description = "DEPRECATED: Use x86_server_spot_request_id"
  value       = length(aws_spot_instance_request.x86_server) > 0 ? aws_spot_instance_request.x86_server[0].id : null
}

output "x86_instance_id" {
  description = "DEPRECATED: Use x86_server_instance_id"
  value = (
    length(aws_instance.x86_server_ondemand) > 0 ? aws_instance.x86_server_ondemand[0].id :
    length(aws_spot_instance_request.x86_server) > 0 ? aws_spot_instance_request.x86_server[0].spot_instance_id :
    null
  )
}

# =============================================================================
# AMI AND VERSION OUTPUTS
# =============================================================================

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
    var.launch_client ? "client" :
    var.launch_arm64_only ? "arm64-only" :
    var.launch_x86_only ? "x86-only" :
    "both"
  )
}

output "server_instance_type_arm64" {
  description = "Instance type used for ARM64 server"
  value       = local.server_instance_types[local.effective_mode].arm64
}

output "server_instance_type_x86" {
  description = "Instance type used for x86 server"
  value       = local.server_instance_types[local.effective_mode].x86
}

output "client_instance_type_arm64" {
  description = "Instance type used for ARM64 client"
  value       = local.client_instance_types[local.effective_mode].arm64
}

output "client_instance_type_x86" {
  description = "Instance type used for x86 client"
  value       = local.client_instance_types[local.effective_mode].x86
}
