# ARM64 Spot Instance
# Created when: not using on-demand AND not launching x86 only
resource "aws_spot_instance_request" "arm64_runner" {
  count = (!var.use_on_demand && !var.launch_x86_only) ? 1 : 0

  ami                    = data.aws_ami.ubuntu_arm64.id
  instance_type          = local.instance_types[local.effective_mode].arm64
  spot_price             = local.spot_prices[local.effective_mode].arm64
  wait_for_fulfillment   = true
  spot_type              = "one-time"

  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  subnet_id              = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata.tftpl", {
    architecture        = "arm64"
    runner_labels       = join(",", local.runner_labels[local.effective_mode].arm64)
    repository_url      = var.repository_url
    gh_pat_runner_token = var.gh_pat_runner_token
    runner_version      = local.runner_version
  })

  root_block_device {
    volume_size           = local.effective_mode == "metal" ? 50 : 20
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name          = "benchmark-runner-arm64-${local.effective_mode}"
    Architecture  = "arm64"
    Mode          = local.effective_mode
    SelfTerminate = "true"
  }

  instance_initiated_shutdown_behavior = "terminate"
}

# x86_64 Spot Instance
# Created when: not using on-demand AND not launching arm64 only
resource "aws_spot_instance_request" "x86_runner" {
  count = (!var.use_on_demand && !var.launch_arm64_only) ? 1 : 0

  ami                    = data.aws_ami.ubuntu_x86.id
  instance_type          = local.instance_types[local.effective_mode].x86
  spot_price             = local.spot_prices[local.effective_mode].x86
  wait_for_fulfillment   = true
  spot_type              = "one-time"

  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  subnet_id              = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata.tftpl", {
    architecture        = "x86_64"
    runner_labels       = join(",", local.runner_labels[local.effective_mode].x86)
    repository_url      = var.repository_url
    gh_pat_runner_token = var.gh_pat_runner_token
    runner_version      = local.runner_version
  })

  root_block_device {
    volume_size           = local.effective_mode == "metal" ? 50 : 20
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name          = "benchmark-runner-x86-${local.effective_mode}"
    Architecture  = "x86_64"
    Mode          = local.effective_mode
    SelfTerminate = "true"
  }

  instance_initiated_shutdown_behavior = "terminate"
}

# ARM64 On-Demand Instance (fallback)
# Created when: using on-demand AND not launching x86 only
resource "aws_instance" "arm64_runner_ondemand" {
  count = (var.use_on_demand && !var.launch_x86_only) ? 1 : 0

  ami           = data.aws_ami.ubuntu_arm64.id
  instance_type = local.instance_types[local.effective_mode].arm64

  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  subnet_id              = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata.tftpl", {
    architecture        = "arm64"
    runner_labels       = join(",", local.runner_labels[local.effective_mode].arm64)
    repository_url      = var.repository_url
    gh_pat_runner_token = var.gh_pat_runner_token
    runner_version      = local.runner_version
  })

  root_block_device {
    volume_size           = local.effective_mode == "metal" ? 50 : 20
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name          = "benchmark-runner-arm64-${local.effective_mode}-ondemand"
    Architecture  = "arm64"
    Mode          = local.effective_mode
    SelfTerminate = "true"
    OnDemand      = "true"
  }

  # Note: instance_initiated_shutdown_behavior removed to avoid IAM permission errors
  # The cleanup job handles termination via terraform destroy
}

# x86_64 On-Demand Instance (fallback)
# Created when: using on-demand AND not launching arm64 only
resource "aws_instance" "x86_runner_ondemand" {
  count = (var.use_on_demand && !var.launch_arm64_only) ? 1 : 0

  ami           = data.aws_ami.ubuntu_x86.id
  instance_type = local.instance_types[local.effective_mode].x86

  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  subnet_id              = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata.tftpl", {
    architecture        = "x86_64"
    runner_labels       = join(",", local.runner_labels[local.effective_mode].x86)
    repository_url      = var.repository_url
    gh_pat_runner_token = var.gh_pat_runner_token
    runner_version      = local.runner_version
  })

  root_block_device {
    volume_size           = local.effective_mode == "metal" ? 50 : 20
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name          = "benchmark-runner-x86-${local.effective_mode}-ondemand"
    Architecture  = "x86_64"
    Mode          = local.effective_mode
    SelfTerminate = "true"
    OnDemand      = "true"
  }

  # Note: instance_initiated_shutdown_behavior removed to avoid IAM permission errors
  # The cleanup job handles termination via terraform destroy
}
