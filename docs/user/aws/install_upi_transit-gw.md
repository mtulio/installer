# Install a cluster with Transit Gateway

Create a Hub-Spoke deployment to use centralized egress-gateway to
deploy OpenShift cluster.

> WIP

```mermaid
graph LR
  role<-.->config
  stsservice<-- AssumeRoleWithWebIdentity/JWT -.->Pod
  Pod-- Creds Assumed Role -->serviceapi
  subgraph cluster [AWS CI Account]
    CloudFront[CloudFront Distribution];
    S3Bucket[S3 Bucket w/ OIDC Config];
    iam_idp-- Public URL -->S3Bucket;
    iam_idp-- Public URL -->CloudFront;
    CloudFront-- Private access -->S3Bucket;
    stsservice -- Trust Tokens<br>Signed -->iam_idp;
    role<-->stsservice;
    subgraph Hub_VPC_(Transit);
      role[IAM Role For Pod];
      iam_idp[IAM Identity Provider/OIDC];
    end
    subgraph AWS_API;
      stsservice[AWS STS API];
      serviceapi[AWS Service API/EC2,S3...];
    end
  end
  subgraph cluster2 [CI Jobs on AWS]
    sa([ServiceAccount Signing Keys]) -- Public <br> Key -->KAS_HTTP;
    sa([ServiceAccount Signing Keys]) -- Private <br> Key -->token_signing;
    sa
    token_signing[Token Signing]-->projected[Projected<br>ServiceAccount<br>Token];
    subgraph KAS;
      KAS_HTTP
      KAS_OIDC[OIDC configs/JWKS]
      KAS_HTTP -- /.well-known/openid-configuration -->KAS_OIDC
      KAS_HTTP -- /openid/v1/jwks -->KAS_OIDC
    end
    subgraph Pod
    config[AWS Config File]-->projected
  end
  end
  classDef plain fill:#ddd,stroke:#fff,stroke-width:4px,color:#000;
  classDef k8s fill:#326ce5,stroke:#fff,stroke-width:4px,color:#fff;
  classDef cluster fill:#fff,stroke:#bbb,stroke-width:2px,color:#326ce5;
  classDef cluster2 fill:#fff,stroke:#bbb,stroke-width:2px,color:#326ce5;
  class config,role,stsservice,serviceapi,projected,token_signing,iam_idp,pod1,pod2,S3Bucket,CloudFront,KAS_HTTP,KAS_OIDC k8s;
  class sa plain;
  class cluster cluster;
  class cluster2 cluster1;
```

## Prerequisites

```sh
RESOURCE_NAME_PREFIX="lab-ci"
AWS_REGION=us-east-1

HUB_VPC_CIDR="172.16.0.0/16"
SPOKE_CIDR_EGRESS="10.0.0.0/8"
```

## Create the Transit Gateway stack

- Transit Gateway
- Transit Gateway Route table

```sh
aws cloudformation create-stack \
    --region ${AWS_REGION} \
    --stack-name ${RESOURCE_NAME_PREFIX}-tgw \
    --template-body file://./01_vpc_01_transit_gateway.yaml \
    --parameters \
        ParameterKey=NamePrefix,ParameterValue=${RESOURCE_NAME_PREFIX}
```

```sh
aws cloudformation wait stack-create-complete \
    --region ${AWS_REGION} \
    --stack-name ${RESOURCE_NAME_PREFIX}-tgw &&\

export TGW_ID=$(aws cloudformation describe-stacks \
  --region ${AWS_REGION} --stack-name ${RESOURCE_NAME_PREFIX}-tgw \
  --query 'Stacks[0].Outputs[?OutputKey==`TransitGatewayId`].OutputValue' \
  --output text)
```

## Create Hub VPC

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



Create the change set:

> TODO --tags

```sh
BUCKET_NAME="installer-upi-templates"
TEMPLATE_BASE_URL="https://${BUCKET_NAME}.s3.amazonaws.com"

aws s3api create-bucket --bucket $BUCKET_NAME --region us-east-1

function update_templates() {
    for TEMPLATE in $(ls *.yaml); do
        if [[ ! "${TEMPLATE}" =~ "01_"* ]]; then
            echo "Ignoring ${TEMPLATE}"
            continue
        fi
        aws s3 cp $TEMPLATE s3://$BUCKET_NAME/${TEMPLATE}
        aws s3api put-object-acl --bucket $BUCKET_NAME --key $TEMPLATE --acl public-read
    done
}
update_templates

aws cloudformation create-change-set \
--stack-name "transit-network-hub" \
--change-set-name "transit-network-hub" \
--change-set-type "CREATE" \
--template-body file://./stack_vpc_transit_hub.yaml \
--include-nested-stacks \
--parameters \
    ParameterKey=NamePrefix,ParameterValue=${RESOURCE_NAME_PREFIX} \
    ParameterKey=PrivateEgressTransitGatewayID,ParameterValue=${TGW_ID} \
    ParameterKey=VpcCidr,ParameterValue=${HUB_VPC_CIDR} \
    ParameterKey=AllowedEgressCidr,ParameterValue=${SPOKE_CIDR_EGRESS} \
    ParameterKey=TemplatesBaseURL,ParameterValue="${TEMPLATE_BASE_URL}"

```

Review the changes.

Execute the stack:

```sh
aws cloudformation execute-change-set \
    --change-set-name "transit-network-hub" \
    --stack-name "transit-network-hub"
```

- Wait

```sh
aws cloudformation wait stack-create-complete \
    --region ${AWS_REGION} \
    --stack-name "transit-network-hub"
```

- Create static route for default route entry in TGW Rtb

```sh
# TODO: TGW module does not provide RTB ID, create custom resource.
TGW_RTB=$(aws ec2 describe-transit-gateway-route-tables --filters Name=transit-gateway-id,Values=$TGW_ID --query 'TransitGatewayRouteTables[].TransitGatewayRouteTableId' --output text)

TGW_ATT_HUB=$(aws cloudformation describe-stacks --stack-name "transit-network-hub" --query 'Stacks[].Outputs[?OutputKey==`TransitGatewayAttachmentId`][].OutputValue' --output text)

aws ec2 create-transit-gateway-route \
--destination-cidr-block "0.0.0.0/0" \
--transit-gateway-route-table-id "${TGW_RTB}" \
--transit-gateway-attachment-id "${TGW_ATT_HUB}"
```

### Delete stack (optional)

```sh
aws cloudformation delete-stack --stack-name "transit-network-hub"
```

## Create Cluster #1 VPC (spoke)

Steps:
- Create regular VPC  (10.0.0.0/16)
- Create private and public subnets across zones
- Create IGW
- Create Transit Gateway attachment
- Create public route table -> IGW
- Create private subnets -> TGW Attch

Deploy:

- Export variables:

```sh
CLUSTER_NAME=c4
CLUSTER_VPC_CIDR=10.10.0.0/16
```

- Create network stacks to install OpenShift with BYO VPC on AWS:

```sh
aws cloudformation create-change-set \
--stack-name "cluster-${CLUSTER_NAME}" \
--change-set-name "cluster-${CLUSTER_NAME}" \
--change-set-type "CREATE" \
--template-body file://./stack_vpc_transit_spoke.yaml \
--include-nested-stacks \
--parameters \
    ParameterKey=NamePrefix,ParameterValue=${CLUSTER_NAME} \
    ParameterKey=PrivateEgressTransitGatewayID,ParameterValue=${TGW_ID} \
    ParameterKey=VpcCidr,ParameterValue=${CLUSTER_VPC_CIDR} \
    ParameterKey=TemplatesBaseURL,ParameterValue="${TEMPLATE_BASE_URL}"
```

- Execute

```sh
aws cloudformation execute-change-set \
    --change-set-name "cluster-${CLUSTER_NAME}" \
    --stack-name "cluster-${CLUSTER_NAME}"
```

- Extract subnet ids

```sh
mapfile -t SUBNETS < <(aws  cloudformation describe-stacks   --stack-name "cluster-${CLUSTER_NAME}" --query 'Stacks[0].Outputs[?OutputKey==`PrivateSubnetIds`].OutputValue' --output text | tr ',' '\n')
mapfile -t -O "${#SUBNETS[@]}" SUBNETS < <(aws  cloudformation describe-stacks   --stack-name "cluster-${CLUSTER_NAME}" --query 'Stacks[0].Outputs[?OutputKey==`PublicSubnetIds`].OutputValue' --output text | tr ',' '\n')

echo ${SUBNETS[*]}
```

- Create install-config.yaml

```sh
export PULL_SECRET_FILE=/path/to/pull-secret
export SSH_PUB_KEY_FILE=${HOME}/.ssh/id_rsa.pub
export BASE_DOMAIN=devcluster.openshift.com
INSTALL_DIR="${HOME}/openshift-labs/${CLUSTER_NAME}"
mkdir $INSTALL_DIR

cat <<EOF > ${INSTALL_DIR}/install-config.yaml
apiVersion: v1
publish: External
baseDomain: ${BASE_DOMAIN}
metadata:
  name: "${CLUSTER_NAME}"
networking:
  machineNetwork:
  - cidr: ${CLUSTER_VPC_CIDR}
platform:
  aws:
    region: ${AWS_REGION}
    subnets:
$(for SB in ${SUBNETS[*]}; do echo "    - $SB"; done)
pullSecret: '$(cat ${PULL_SECRET_FILE} | awk -v ORS= -v OFS= '{$1=$1}1')'
sshKey: |
  $(cat ${SSH_PUB_KEY_FILE})
EOF
```

- Create cluster

```sh
openshift-install create cluster --dir ${INSTALL_DIR} --log-level=debug
```

- Review installation

```sh
export KUBECONFIG=${INSTALL_DIR}/auth/kubeconfig
oc get nodes
```

```text
$ oc get clusterversion
NAME      VERSION       AVAILABLE   PROGRESSING   SINCE   STATUS
version   4.15.0-rc.0   True        False         2m30s   Cluster version is 4.15.0-rc.0
```


### Destroy (optional)

- Destroy cluster

```sh
openshift-install destroy cluster --dir ${INSTALL_DIR} --log-level=debug
```

- Network

```sh
aws cloudformation delete-stack \
    --stack-name "cluster-${CLUSTER_NAME}"
```

## Create Cluster #2 VPC (spoke)

> TBD

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