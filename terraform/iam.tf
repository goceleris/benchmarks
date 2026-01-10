# IAM Role for benchmark runner instances
resource "aws_iam_role" "benchmark_runner" {
  name = "benchmark_runner_role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Action = "sts:AssumeRole"
        Effect = "Allow"
        Principal = {
          Service = "ec2.amazonaws.com"
        }
      }
    ]
  })
}

# Policy allowing instance to terminate itself
resource "aws_iam_role_policy" "self_terminate" {
  name = "self_terminate_policy"
  role = aws_iam_role.benchmark_runner.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "ec2:TerminateInstances"
        ]
        Resource = "arn:aws:ec2:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:instance/*"
        Condition = {
          StringEquals = {
            "ec2:ResourceTag/SelfTerminate" = "true"
          }
        }
      },
      {
        Effect = "Allow"
        Action = [
          "ec2:DescribeInstances",
          "ec2:DescribeTags"
        ]
        Resource = "*"
      }
    ]
  })
}

# Instance profile to attach the role to EC2 instances
resource "aws_iam_instance_profile" "benchmark_runner" {
  name = "benchmark_runner_profile"
  role = aws_iam_role.benchmark_runner.name
}
