# Install a cluster with Transit Gateway

Create a Hub-Spoke deployment to use centralized egress-gateway to
deploy OpenShift cluster.

## Create Hub VPC

- Transit Gateway
- Transit Gateway Route table

- Create regular VPC (172.254.0.0/16)
- Create a private and public subnets across zones
- Create IGW
- Create a single Nat GW
- Create public route table -> IGW
- Create private route table -> NGW

- Create Transit Gateway attachment
- Create TGW default route to TG attachment in TG RTB
- Create route entry to 10.0.0.0/8 to Attachment in both public and private route tables for the VPC

- Deploy mirror registry into hub VPC in public subnet
- Create DNS

## Create Cluster #1 VPC (spoke)

- Create regular VPC  (10.0.0.0/16)
- Create private and public subnets across zones
- Create IGW
- Create Transit Gateway attachment
- Create public route table -> IGW
- Create private subnets -> TGW Attch

## Create Cluster #2 VPC (spoke)

- Create regular VPC  (10.1.0.0/16)
- Create private and public subnets across zones
- Create IGW
- Create Transit Gateway attachment
- Create public route table -> IGW
- Create private subnets -> TGW Attch


## References

- https://docs.aws.amazon.com/vpc/latest/tgw/transit-gateway-nat-igw.html
- https://aws.amazon.com/blogs/networking-and-content-delivery/using-nat-gateways-with-multiple-amazon-vpcs-at-scale/

- Stack Set nested cloudformation template: https://curiousorbit.com/blog/aws-cloudformation-nested-stacks/