# Security group for benchmark runners
resource "aws_security_group" "benchmark_runner" {
  name        = "benchmark-runner-sg-${var.benchmark_mode}"
  description = "Security group for ${var.benchmark_mode} benchmark runner instances"

  # Allow all outbound traffic (needed for GitHub API, package downloads)
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "benchmark-runner-sg-${var.benchmark_mode}"
    Mode = var.benchmark_mode
  }
}

# ARM64 Spot Instance
resource "aws_spot_instance_request" "benchmark_arm64" {
  ami                    = data.aws_ami.ubuntu_arm64.id
  instance_type          = local.instance_types[var.benchmark_mode].arm64
  spot_price             = local.spot_prices[var.benchmark_mode].arm64
  wait_for_fulfillment   = true
  spot_type              = "one-time"
  iam_instance_profile   = aws_iam_instance_profile.benchmark_runner.name
  vpc_security_group_ids = [aws_security_group.benchmark_runner.id]

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
  iam_instance_profile   = aws_iam_instance_profile.benchmark_runner.name
  vpc_security_group_ids = [aws_security_group.benchmark_runner.id]

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
