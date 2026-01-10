# ARM64 Spot Instance
resource "aws_spot_instance_request" "benchmark_arm64" {
  ami                    = data.aws_ami.ubuntu_arm64.id
  instance_type          = local.instance_types[var.benchmark_mode].arm64
  spot_price             = local.spot_prices[var.benchmark_mode].arm64
  wait_for_fulfillment   = true
  spot_type              = "one-time"
  
  # Use pre-existing IAM instance profile (optional)
  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  
  # Use pre-existing security group if provided, otherwise use default
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  
  # Use specific subnet if provided
  subnet_id = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata.tftpl", {
    architecture        = "arm64"
    runner_labels       = join(",", local.runner_labels[var.benchmark_mode].arm64)
    repository_url      = var.repository_url
    gh_pat_runner_token = var.gh_pat_runner_token
    runner_version      = local.runner_version
  })

  root_block_device {
    volume_size           = var.benchmark_mode == "metal" ? 50 : 20
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name          = "benchmark-runner-arm64-${var.benchmark_mode}"
    Architecture  = "arm64"
    Mode          = var.benchmark_mode
    SelfTerminate = "true"
  }

  instance_initiated_shutdown_behavior = "terminate"
}

# x86_64 Spot Instance
resource "aws_spot_instance_request" "benchmark_x86" {
  ami                    = data.aws_ami.ubuntu_x86.id
  instance_type          = local.instance_types[var.benchmark_mode].x86
  spot_price             = local.spot_prices[var.benchmark_mode].x86
  wait_for_fulfillment   = true
  spot_type              = "one-time"
  
  # Use pre-existing IAM instance profile (optional)
  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  
  # Use pre-existing security group if provided, otherwise use default
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  
  # Use specific subnet if provided
  subnet_id = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata.tftpl", {
    architecture        = "x86_64"
    runner_labels       = join(",", local.runner_labels[var.benchmark_mode].x86)
    repository_url      = var.repository_url
    gh_pat_runner_token = var.gh_pat_runner_token
    runner_version      = local.runner_version
  })

  root_block_device {
    volume_size           = var.benchmark_mode == "metal" ? 50 : 20
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name          = "benchmark-runner-x86-${var.benchmark_mode}"
    Architecture  = "x86_64"
    Mode          = var.benchmark_mode
    SelfTerminate = "true"
  }

  instance_initiated_shutdown_behavior = "terminate"
}
