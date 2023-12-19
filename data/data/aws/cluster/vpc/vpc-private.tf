locals {
  crerate_egress_natgw = contains(values(var.edge_zones_type), "wavelength-zone")
}

resource "aws_route_table" "private_routes" {
  count = var.private_subnets == null ? length(var.availability_zones) : 0

  vpc_id = data.aws_vpc.cluster_vpc.id

  tags = merge(
    {
      "Name" = "${var.cluster_id}-private-${var.availability_zones[count.index]}"
    },
    var.tags,
  )
}

resource "aws_route" "to_nat_gw" {
  count = local.has_nat_gw ? length(var.availability_zones) : 0

  route_table_id         = aws_route_table.private_routes[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = element(aws_nat_gateway.nat_gw.*.id, count.index)
  depends_on             = [aws_route_table.private_routes]

  timeouts {
    create = "20m"
  }
}

resource "aws_route" "to_transit_gw" {
  count = local.has_transit_gw ? length(var.availability_zones) : 0

  route_table_id         = aws_route_table.private_routes[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  transit_gateway_id     = var.private_egress_tgw
  depends_on             = [aws_ec2_transit_gateway_vpc_attachment.private]

  timeouts {
    create = "10m"
  }
}

resource "aws_subnet" "private_subnet" {
  count = var.private_subnets == null ? length(var.availability_zones) : 0

  vpc_id = data.aws_vpc.cluster_vpc.id

  cidr_block = cidrsubnet(local.new_private_cidr_range, ceil(log(length(var.availability_zones), 2)), count.index)

  availability_zone = var.availability_zones[count.index]

  tags = merge(
    {
      "Name"                            = "${var.cluster_id}-private-${var.availability_zones[count.index]}"
      "kubernetes.io/role/internal-elb" = ""
    },
    var.tags,
  )
}

resource "aws_subnet" "edge_private_subnet" {
  count = var.edge_zones == null ? 0 : length(var.edge_zones)

  vpc_id            = data.aws_vpc.cluster_vpc.id
  cidr_block        = cidrsubnet(local.new_edge_private_cidr_range, ceil(log(length(var.edge_zones), 2)), count.index)
  availability_zone = var.edge_zones[count.index]

  tags = merge(
    {
      "Name" = "${var.cluster_id}-private-${var.edge_zones[count.index]}"
    },
    var.tags,
  )

}

resource "aws_route_table_association" "private_routing" {
  count = var.private_subnets == null ? length(var.availability_zones) : 0

  route_table_id = aws_route_table.private_routes[count.index].id
  subnet_id      = aws_subnet.private_subnet[count.index].id
}

resource "aws_route_table_association" "edge_private_routing" {
  count = var.edge_zones == null ? 0 : length(var.edge_zones)

  # Lookup the index of the parent zone from a given Local Zone name,
  # getting the index for the route table id for that zone (parent),
  # when not found (parent zone's gateway does not exists), the first
  # route table will be used.
  # Example edge_parent_gw_map = {us-east-1-nyc-1a=0}
  route_table_id = aws_route_table.private_routes[lookup(var.edge_parent_gw_map, aws_subnet.edge_private_subnet[count.index].availability_zone, 0)].id
  subnet_id      = aws_subnet.edge_private_subnet[count.index].id
}

resource "aws_ec2_transit_gateway_vpc_attachment" "private" {
  subnet_ids         = aws_subnet.private_subnet[*].id
  transit_gateway_id = var.private_egress_tgw
  vpc_id             = data.aws_vpc.cluster_vpc.id

  tags = merge(
    {
      "Name"                            = "${var.cluster_id}-tgw-attach"
    },
    var.tags,
  )
}
