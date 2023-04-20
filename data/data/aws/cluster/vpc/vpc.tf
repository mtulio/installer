locals {
  # The VPC CIDR block is split initially into two blocks: private and public subnets.
  # The private subnet is dedicated to the sliced-into N*Zones with those conditions
  # - when three or more zones are defined to the cluster, the private blocks will use 100% to expand the subnets
  # - when there are defined two or fewer zones for master and workers, only 50% of the remaining CIDR block will
  #   be used to create subnets, allowing spare blocks to users create more subnets in day-2
  # - when edge subnets are defined, it will get 50% of public blocks. It is using only 50% to create subnets,
  #   allowing to expand to new locations when the cluster is installed with few locations.


  # Allowing expansion and avoiding allocating all CIDR blocks from VPC without leading
  # space (free CIDR blocks) for maintenance.
  # Considering the maximum nodes on cluster are 2k, and the default deployment with 1 zone
  # is deploying a single subnet with 32K IPs (/17), that limitation should not impact the
  # current behavior.
  allow_expansion_zones = length(var.availability_zones) == 1 ? 1 : 0
  allow_expansion_edge  = length(var.edge_zones) == 1 ? 1 : 0

  edge_enabled = length(var.edge_zones) > 0 ? 1 : 0

  # Slice the VPC CIDR into two blocks (50%) [default behavior].
  vpc_cidr_50_1 = cidrsubnet(data.aws_vpc.cluster_vpc.cidr_block, 1, 0)
  vpc_cidr_50_2 = cidrsubnet(data.aws_vpc.cluster_vpc.cidr_block, 1, 1)

  # Slice the Second CIDR block into two (25% of VPC CIDR), when edge zone is added.
  # When an edge is enabled, the second half will be sliced to accommodate edge subnets.
  vpc_cidr_50_2_50_1 = cidrsubnet(local.vpc_cidr_50_2, 1, 0)
  vpc_cidr_50_2_50_2 = cidrsubnet(local.vpc_cidr_50_2, 1, 1)

  new_private_cidr_range = cidrsubnet(local.vpc_cidr_50_1, local.allow_expansion_zones, 0)
  new_public_cidr_range  = local.edge_enabled == 0 ? cidrsubnet(local.vpc_cidr_50_2, local.allow_expansion_zones, 0) : cidrsubnet(local.vpc_cidr_50_2_50_1, local.allow_expansion_zones, 0)
  new_edge_cidr_range    = local.allow_expansion_edge == 0 ? local.vpc_cidr_50_2_50_2 : cidrsubnet(local.vpc_cidr_50_2_50_2, local.allow_expansion_edge, 0)
}

resource "aws_vpc" "new_vpc" {
  count = var.vpc == null ? 1 : 0

  cidr_block           = var.cidr_blocks[0]
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = merge(
    {
      "Name" = "${var.cluster_id}-vpc"
    },
    var.tags,
  )
}

resource "aws_vpc_endpoint" "s3" {
  count = var.vpc == null ? 1 : 0

  vpc_id       = data.aws_vpc.cluster_vpc.id
  service_name = "com.amazonaws.${var.region}.s3"
  route_table_ids = concat(
    aws_route_table.private_routes.*.id,
    aws_route_table.default.*.id,
  )

  tags = var.tags
}

resource "aws_vpc_dhcp_options" "main" {
  count = var.vpc == null ? 1 : 0

  domain_name         = var.region == "us-east-1" ? "ec2.internal" : format("%s.compute.internal", var.region)
  domain_name_servers = ["AmazonProvidedDNS"]

  tags = var.tags
}

resource "aws_vpc_dhcp_options_association" "main" {
  count = var.vpc == null ? 1 : 0

  vpc_id          = data.aws_vpc.cluster_vpc.id
  dhcp_options_id = aws_vpc_dhcp_options.main[0].id
}
