# Install OpenShift on AWS using owned Public IPv4 Pool (BYO Public IPv4)

Steps to create a cluster on AWS using Public IPv4 address pool
that you brought to your AWS account with BYO Public IPv4.

## Prerequisites

- Public IPv4 Pool must be provisioned and advertised in the AWS Account. See more on AWS Documentation to "[Onboard your BYOIP](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-byoip.html#byoip-onboard)"
- Additional permissions must be added to the user running openshift-install: `ec2:DescribePublicIpv4Pools` and `ec2:DisassociateAddress`
- Total of ( (Zones*3 ) + 1) of Public IPv4 available in the pool, where: Zones is the total numbber of AWS zones used to deploy the OpenShift cluster.
    - Example to query the IPv4 pools available in the account, which returns the  `TotalAvailableAddressCount`:

```sh
$ aws ec2 describe-public-ipv4-pools --region us-east-1
{
    "PublicIpv4Pools": [
        {
            "PoolId": "ipv4pool-ec2-012456789abcdef00",
            "Description": "",
            "PoolAddressRanges": [
                {
                    "FirstAddress": "157.254.254.0",
                    "LastAddress": "157.254.254.255",
                    "AddressCount": 256,
                    "AvailableAddressCount": 83
                }
            ],
            "TotalAddressCount": 256,
            "TotalAvailableAddressCount": 83,
            "NetworkBorderGroup": "us-east-1",
            "Tags": []
        }
    ]
}
```

## Steps

- Create the install config setting the field `platform.aws.publicIpv4Pool`, and create the cluster:

```yaml
apiVersion: v1
baseDomain: ${CLUSTER_BASE_DOMAIN}
metadata:
  name: ocp-byoip
platform:
  aws:
    region: ${REGION}
    publicIpv4Pool: ipv4pool-ec2-012456789abcdef00
publish: External
pullSecret: '...'
sshKey: |
  '...'
```

- Create the cluster

```sh
openshift-install create cluster
```

# Install OpenShift on AWS using existing Public IPv4 Address (BYO Elastic IP (EIP))

Steps to install OpenShift on AWS using existing Elastic IPs when creating
network resources (Nat Gateways, APi's NLB, Bootstrap).

## Prerequisites

- The Elastic IPs must be allocated and not associated to any resource.
- Each Elastic IP allocation must have the correct tags by role to match the provisioner lookup (CAPA).

### Example allocating Elastic IP Addresses

> Following suggestion from EP: https://github.com/openshift/enhancements/pull/1593#discussion_r1677263221
> More details of existing tests using EIP allocation: https://github.com/openshift/installer/pull/8175#issuecomment-2111229833

## Steps

- Generate the basic install-config.yaml to discover the zones:

```sh
INSTALLER=./openshift-install-devel
#export RELEASE_IMAGE=quay.io/openshift-release-dev/ocp-release:4.17.0-rc.0-x86_64
export RELEASE_IMAGE=registry.ci.openshift.org/ocp/release:4.18.0-0.nightly-2024-08-28-140146

REGION=us-east-1
CLUSTER_BASE_DOMAIN=devcluster.openshift.com
CLUSTER_NAME=byoeip-v11

INSTALL_DIR=./install-dir-${CLUSTER_NAME}
mkdir -p ${INSTALL_DIR}

cat << EOF > ${INSTALL_DIR}/install-config.yaml
apiVersion: v1
publish: External
baseDomain: ${CLUSTER_BASE_DOMAIN}
metadata:
  name: ${CLUSTER_NAME}
platform:
  aws:
    region: ${REGION}
pullSecret: '$(cat ${PULL_SECRET_FILE})'
sshKey: |
  $(cat ~/.ssh/id_rsa.pub)
EOF

${INSTALLER} create manifests --dir=${INSTALL_DIR}
```

- Discover the total of zones the cluster will be deployed, if none is defined in the original install-config.yaml:

~~~sh
ZONE_COUNT=$(yq ea '.spec.network.subnets[] | [select(.isPublic==true).availabilityZone] | length' ${INSTALL_DIR}/cluster-api/02_infra-cluster.yaml)
~~~

- Discover the InfraID:

```sh
CLUSTER_ID=$(yq ea .status.infrastructureName ${INSTALL_DIR}/manifests/cluster-infrastructure-02-config.yml)

# Create cluster tags (must have the 'shared' value to be unmanaged)
CLUSTER_TAGS="{Key=kubernetes.io/cluster/${CLUSTER_ID},Value=shared}"
CLUSTER_TAGS+=",{Key=sigs.k8s.io/cluster-api-provider-aws/cluster/${CLUSTER_ID},Value=shared}"
```

- Allocate addresses for `NatGateways` (role==common) setting the cluster api tags:

```sh
# 'common' role is an standard for Cluster API AWS for EIPs when
# creating Nat Gateways for private subnets.
TAG_ROLE=common

TAGS="{Key=Name,Value=${CLUSTER_ID}-eip-${TAG_ROLE}}"
TAGS+=",${CLUSTER_TAGS}"

# Cluster API will look up for the role and assign to the resource
TAGS+=",{Key=sigs.k8s.io/cluster-api-provider-aws/role,Value=${TAG_ROLE}}"

# Allocate the addresses
for EIP_ID in $(seq 1 ${ZONE_COUNT}); do
  aws --region ${REGION} ec2 allocate-address \
    --domain "vpc" \
    --tag-specifications "ResourceType=elastic-ip,Tags=[${TAGS}]" \
    | tee -a ${INSTALL_DIR}/eips-${TAG_ROLE}.txt
done
```

- Allocate addresses for `NatGateways` (role==common) setting the cluster api tags:

```sh
# 'lb-apiserver' role is an standard match for Cluster API AWS for EIPs when
# creating Public Load Balancer for API.
TAG_ROLE=lb-apiserver

TAGS="{Key=Name,Value=${CLUSTER_ID}-eip-${TAG_ROLE}}"
TAGS+=",${CLUSTER_TAGS}"

# Cluster API will look up for the role and assign to the resource
TAGS+=",{Key=sigs.k8s.io/cluster-api-provider-aws/role,Value=${TAG_ROLE}}"

# Allocate the addresses
for EIP_ID in $(seq 1 ${ZONE_COUNT}); do
  aws --region ${REGION} ec2 allocate-address \
    --domain "vpc" \
    --tag-specifications "ResourceType=elastic-ip,Tags=[${TAGS}]" \
    | tee -a ${INSTALL_DIR}/eips-${TAG_ROLE}.txt
done
```

- Allocate addresses for `Machines` (role==ec2-custom) setting the cluster api tags:

```sh
# 'ec2-custom' role is an standard match for Cluster API AWS for EIPs when
# creating machines in Public subnets.
TAG_ROLE=ec2-custom

TAGS="{Key=Name,Value=${CLUSTER_ID}-eip-${TAG_ROLE}}"
TAGS+=",${CLUSTER_TAGS}"

# Cluster API will look up for the role and assign to the resource
TAGS+=",{Key=sigs.k8s.io/cluster-api-provider-aws/role,Value=${TAG_ROLE}}"

# Allocate the addresses
aws --region ${REGION} ec2 allocate-address \
  --domain "vpc" \
  --tag-specifications "ResourceType=elastic-ip,Tags=[${TAGS}]" \
  | tee -a ${INSTALL_DIR}/eips-${TAG_ROLE}.txt
```

- Create the cluster

```sh
${INSTALLER} create cluster --dir=${INSTALL_DIR} --log-level=debug
```

### Reviewing the EIPs

Checking if the cluster has been created re-using the pre-allocated EIPs:

- Check the Public IPs for Nat Gateway:

```sh
$ jq -r .PublicIp ${INSTALL_DIR}/eips-common.txt | sort -n
34.194.161.249
44.218.180.88
44.222.26.11
52.44.237.214
54.144.209.129
54.236.196.217

$ aws ec2 describe-nat-gateways --filter Name=tag-key,Values=kubernetes.io/cluster/${CLUSTER_ID} | jq -r .NatGateways[].NatGatewayAddresses[].PublicIp | sort -n
34.194.161.249
44.218.180.88
44.222.26.11
52.44.237.214
54.144.209.129
54.236.196.217
```

- Check the Addresses for API's NLB:

> BAH! Bug! CAPA is not assigning BYO EIPs

```sh
$ jq -r .PublicIp ${INSTALL_DIR}/eips-lb-apiserver.txt | sort -n
3.233.6.197
3.91.167.197
34.198.58.67
44.218.195.77
54.156.68.110
54.210.212.77

$ dig +short api.ocp-byoeip.${CLUSTER_BASE_DOMAIN} | sort -n
23.23.33.250
34.206.145.141
35.172.27.105
44.216.208.39
54.225.119.195
100.29.105.247
```

### Caveats

TBD:
- Do we need to store the EIP allocations, or set custom tags, to the BYO EIPs? (example setting `openshift_creationDate` tag). If so, the install-config.yaml entry must be added
- What about the EIP for bootstrap? Is it required to support in CORS-2603?




=============================

>>>>>>>>>>> REMOVE
> Useless, should not have anything to change on installer since CAPA will discover it. Need to run tests.

- Create the install config setting the  `platform.aws.elasticIpPools`:

```sh
EIPS_NAT_GATEWAY=$(jq -r .AllocationId ${INSTALL_DIR}/eips-common.txt | tr '\n' ',')
EIPS_NAT_GATEWAY=${EIPS_NAT_GATEWAY::-1}

EIPS_API_LB=$(jq -r .AllocationId ${INSTALL_DIR}/eips-lb-apiserver.txt | tr '\n' ',')
EIPS_API_LB=${EIPS_API_LB::-1}

cat << EOF > ${INSTALL_DIR}/patch-install-config.yaml
platform:
  aws:
    eipAllocations:
      natGateway: [${EIPS_NAT_GATEWAY}]
      apiNetworkLoadBalancer: [${EIPS_API_LB::-1}]
EOF

${INSTALLER} create install-config --dir=${INSTALL_DIR}

# patch install-config.yaml
yq4 ea --inplace '. as $item ireduce ({}; . * $item )' \
    ${INSTALL_DIR}/install-config.yaml ${INSTALL_DIR}/patch-install-config.yaml
```
<<<<<<<<<<

Allocating address pool for NAT Gateways:
~~~sh

~~~

Allocating address pool for API Network Load Balancer:
~~~sh

~~~

Allocating address for Bootstrap node:
~~~sh
TBD
~~~

Allocating address for Ingress node:
~~~sh
TBD
~~~
