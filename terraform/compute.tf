# =============================================================================
# SERVER INSTANCES
# These run the HTTP servers being benchmarked.
# Spot instances with on-demand fallback for cost efficiency.
# =============================================================================

# ARM64 Server - Spot Instance
# Created when: not using on-demand AND not launching x86 only AND not launching client only
resource "aws_spot_instance_request" "arm64_server" {
  count = (!var.use_on_demand && !var.launch_x86_only && !var.launch_client) ? 1 : 0

  ami                    = data.aws_ami.ubuntu_arm64.id
  instance_type          = local.server_instance_types[local.effective_mode].arm64
  spot_price             = local.spot_prices[local.effective_mode].arm64
  wait_for_fulfillment   = true
  spot_type              = "one-time"
  availability_zone      = local.availability_zone

  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  subnet_id              = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata.tftpl", {
    architecture        = "arm64"
    runner_labels       = join(",", local.server_runner_labels[local.effective_mode].arm64)
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
    Name          = "benchmark-server-arm64-${local.effective_mode}"
    Architecture  = "arm64"
    Mode          = local.effective_mode
    Role          = "server"
    SelfTerminate = "true"
  }

  instance_initiated_shutdown_behavior = "terminate"
}

# x86_64 Server - Spot Instance
# Created when: not using on-demand AND not launching arm64 only AND not launching client only
resource "aws_spot_instance_request" "x86_server" {
  count = (!var.use_on_demand && !var.launch_arm64_only && !var.launch_client) ? 1 : 0

  ami                    = data.aws_ami.ubuntu_x86.id
  instance_type          = local.server_instance_types[local.effective_mode].x86
  spot_price             = local.spot_prices[local.effective_mode].x86
  wait_for_fulfillment   = true
  spot_type              = "one-time"
  availability_zone      = local.availability_zone

  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  subnet_id              = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata.tftpl", {
    architecture        = "x86_64"
    runner_labels       = join(",", local.server_runner_labels[local.effective_mode].x86)
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
    Name          = "benchmark-server-x86-${local.effective_mode}"
    Architecture  = "x86_64"
    Mode          = local.effective_mode
    Role          = "server"
    SelfTerminate = "true"
  }

  instance_initiated_shutdown_behavior = "terminate"
}

# ARM64 Server - On-Demand Instance (fallback)
# Created when: using on-demand AND not launching x86 only AND not launching client only
resource "aws_instance" "arm64_server_ondemand" {
  count = (var.use_on_demand && !var.launch_x86_only && !var.launch_client) ? 1 : 0

  ami               = data.aws_ami.ubuntu_arm64.id
  instance_type     = local.server_instance_types[local.effective_mode].arm64
  availability_zone = local.availability_zone

  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  subnet_id              = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata.tftpl", {
    architecture        = "arm64"
    runner_labels       = join(",", local.server_runner_labels[local.effective_mode].arm64)
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
    Name          = "benchmark-server-arm64-${local.effective_mode}-ondemand"
    Architecture  = "arm64"
    Mode          = local.effective_mode
    Role          = "server"
    SelfTerminate = "true"
    OnDemand      = "true"
  }
}

# x86_64 Server - On-Demand Instance (fallback)
# Created when: using on-demand AND not launching arm64 only AND not launching client only
resource "aws_instance" "x86_server_ondemand" {
  count = (var.use_on_demand && !var.launch_arm64_only && !var.launch_client) ? 1 : 0

  ami               = data.aws_ami.ubuntu_x86.id
  instance_type     = local.server_instance_types[local.effective_mode].x86
  availability_zone = local.availability_zone

  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  subnet_id              = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata.tftpl", {
    architecture        = "x86_64"
    runner_labels       = join(",", local.server_runner_labels[local.effective_mode].x86)
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
    Name          = "benchmark-server-x86-${local.effective_mode}-ondemand"
    Architecture  = "x86_64"
    Mode          = local.effective_mode
    Role          = "server"
    SelfTerminate = "true"
    OnDemand      = "true"
  }
}

# =============================================================================
# CLIENT INSTANCES
# These run the benchmark tool and connect to server instances.
# Always on-demand for stability (no spot termination mid-benchmark).
# =============================================================================

# ARM64 Client - On-Demand Instance
# Created when: launching client AND not launching x86 only
resource "aws_instance" "arm64_client" {
  count = (var.launch_client && !var.launch_x86_only) ? 1 : 0

  ami               = data.aws_ami.ubuntu_arm64.id
  instance_type     = local.client_instance_types[local.effective_mode].arm64
  availability_zone = local.availability_zone

  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  subnet_id              = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata-client.tftpl", {
    architecture        = "arm64"
    runner_labels       = join(",", local.client_runner_labels[local.effective_mode].arm64)
    repository_url      = var.repository_url
    gh_pat_runner_token = var.gh_pat_runner_token
    runner_version      = local.runner_version
    server_ip           = var.server_ip_arm64
  })

  root_block_device {
    volume_size           = 20
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name          = "benchmark-client-arm64-${local.effective_mode}"
    Architecture  = "arm64"
    Mode          = local.effective_mode
    Role          = "client"
    SelfTerminate = "true"
    OnDemand      = "true"
  }
}

# x86_64 Client - On-Demand Instance
# Created when: launching client AND not launching arm64 only
resource "aws_instance" "x86_client" {
  count = (var.launch_client && !var.launch_arm64_only) ? 1 : 0

  ami               = data.aws_ami.ubuntu_x86.id
  instance_type     = local.client_instance_types[local.effective_mode].x86
  availability_zone = local.availability_zone

  iam_instance_profile   = var.iam_instance_profile_name != "" ? var.iam_instance_profile_name : null
  vpc_security_group_ids = var.security_group_id != "" ? [var.security_group_id] : null
  subnet_id              = var.subnet_id != "" ? var.subnet_id : null

  user_data = templatefile("${path.module}/userdata-client.tftpl", {
    architecture        = "x86_64"
    runner_labels       = join(",", local.client_runner_labels[local.effective_mode].x86)
    repository_url      = var.repository_url
    gh_pat_runner_token = var.gh_pat_runner_token
    runner_version      = local.runner_version
    server_ip           = var.server_ip_x86
  })

  root_block_device {
    volume_size           = 20
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name          = "benchmark-client-x86-${local.effective_mode}"
    Architecture  = "x86_64"
    Mode          = local.effective_mode
    Role          = "client"
    SelfTerminate = "true"
    OnDemand      = "true"
  }
}

# =============================================================================
# BACKWARDS COMPATIBILITY ALIASES
# These allow existing workflows to work during migration
# =============================================================================

# Alias old resource names for backwards compatibility
resource "aws_spot_instance_request" "arm64_runner" {
  count = 0  # Disabled - use arm64_server instead
  ami           = data.aws_ami.ubuntu_arm64.id
  instance_type = "t4g.micro"
  spot_price    = "0.01"
}

resource "aws_spot_instance_request" "x86_runner" {
  count = 0  # Disabled - use x86_server instead
  ami           = data.aws_ami.ubuntu_x86.id
  instance_type = "t3.micro"
  spot_price    = "0.01"
}

resource "aws_instance" "arm64_runner_ondemand" {
  count = 0  # Disabled - use arm64_server_ondemand instead
  ami           = data.aws_ami.ubuntu_arm64.id
  instance_type = "t4g.micro"
}

resource "aws_instance" "x86_runner_ondemand" {
  count = 0  # Disabled - use x86_server_ondemand instead
  ami           = data.aws_ami.ubuntu_x86.id
  instance_type = "t3.micro"
}
