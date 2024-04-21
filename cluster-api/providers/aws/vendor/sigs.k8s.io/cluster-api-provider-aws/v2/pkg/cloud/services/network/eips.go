/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package network

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pkg/errors"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/awserrors"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/filter"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/wait"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/tags"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/record"
)

func (s *Service) getOrAllocateAddresses(num int, role string) (eips []string, err error) {
	out, err := s.describeAddresses(role)
	if err != nil {
		record.Eventf(s.scope.InfraCluster(), "FailedDescribeAddresses", "Failed to query addresses for role %q: %v", role, err)
		return nil, errors.Wrap(err, "failed to query addresses")
	}

	// Reuse existing unallocated addreses with the same role.
	for _, address := range out.Addresses {
		if address.AssociationId == nil {
			eips = append(eips, aws.StringValue(address.AllocationId))
		}
	}

	for len(eips) < num {
		ip, err := s.allocateAddress(role)
		if err != nil {
			return nil, err
		}
		eips = append(eips, ip)
	}

	return eips, nil
}

func (s *Service) allocateAddress(role string) (string, error) {
	tagSpecifications := tags.BuildParamsToTagSpecification(ec2.ResourceTypeElasticIp, s.getEIPTagParams(role))
	allocInput := &ec2.AllocateAddressInput{
		Domain: aws.String("vpc"),
		TagSpecifications: []*ec2.TagSpecification{
			tagSpecifications,
		},
	}

	if s.scope.VPC().PublicIpv4Pool != nil {
		ok, err := s.publicIpv4PoolHasFreeIPs(1)
		if err != nil {
			record.Warnf(s.scope.InfraCluster(), "FailedAllocateEIP", "Failed to allocate Elastic IP for %q in Public IPv4 Pool %s", role, s.scope.VPC().PublicIpv4Pool)
			return "", errors.New("failed to allocate Elastic IP from PublicIpv4 Pool")
		}
		if !ok && s.scope.VPC().PublicIpv4PoolFallBackOrder != nil && s.scope.VPC().PublicIpv4PoolFallBackOrder.Equal(infrav1.PublicIpv4PoolFallbackOrderNone) {
			record.Warnf(s.scope.InfraCluster(), "FailedAllocateEIPFromBYOIP", "Failed to allocate Elastic IP for %q in Public IPv4 Pool %s and fallback isnt enabled//", role, s.scope.VPC().PublicIpv4Pool)
			return "", fmt.Errorf("failed to allocate Elastic IP from PublicIpv4 Pool and use fallback with strategy %s", *s.scope.VPC().PublicIpv4PoolFallBackOrder)
		}
		allocInput.PublicIpv4Pool = s.scope.VPC().PublicIpv4Pool
	}

	out, err := s.EC2Client.AllocateAddressWithContext(context.TODO(), allocInput)
	if err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedAllocateEIP", "Failed to allocate Elastic IP for %q: %v", role, err)
		return "", errors.Wrap(err, "failed to allocate Elastic IP")
	}
	return aws.StringValue(out.AllocationId), nil
}

func (s *Service) describeAddresses(role string) (*ec2.DescribeAddressesOutput, error) {
	x := []*ec2.Filter{filter.EC2.Cluster(s.scope.Name())}
	if role != "" {
		x = append(x, filter.EC2.ProviderRole(role))
	}

	return s.EC2Client.DescribeAddressesWithContext(context.TODO(), &ec2.DescribeAddressesInput{
		Filters: x,
	})
}

func (s *Service) disassociateAddress(ip *ec2.Address) error {
	err := wait.WaitForWithRetryable(wait.NewBackoff(), func() (bool, error) {
		_, err := s.EC2Client.DisassociateAddressWithContext(context.TODO(), &ec2.DisassociateAddressInput{
			AssociationId: ip.AssociationId,
		})
		if err != nil {
			cause, _ := awserrors.Code(errors.Cause(err))
			if cause != awserrors.AssociationIDNotFound {
				return false, err
			}
		}
		return true, nil
	}, awserrors.AuthFailure)
	if err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedDisassociateEIP", "Failed to disassociate Elastic IP %q: %v", *ip.AllocationId, err)
		return errors.Wrapf(err, "failed to disassociate Elastic IP %q", *ip.AllocationId)
	}
	return nil
}

func (s *Service) releaseAddresses() error {
	out, err := s.EC2Client.DescribeAddressesWithContext(context.TODO(), &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{filter.EC2.Cluster(s.scope.Name())},
	})
	if err != nil {
		return errors.Wrapf(err, "failed to describe elastic IPs %q", err)
	}
	if out == nil {
		return nil
	}
	for i := range out.Addresses {
		ip := out.Addresses[i]
		if ip.AssociationId != nil {
			if _, err := s.EC2Client.DisassociateAddressWithContext(context.TODO(), &ec2.DisassociateAddressInput{
				AssociationId: ip.AssociationId,
			}); err != nil {
				record.Warnf(s.scope.InfraCluster(), "FailedDisassociateEIP", "Failed to disassociate Elastic IP %q: %v", *ip.AllocationId, err)
				return errors.Errorf("failed to disassociate Elastic IP %q with allocation ID %q: Still associated with association ID %q", *ip.PublicIp, *ip.AllocationId, *ip.AssociationId)
			}
		}

		if err := wait.WaitForWithRetryable(wait.NewBackoff(), func() (bool, error) {
			_, err := s.EC2Client.ReleaseAddressWithContext(context.TODO(), &ec2.ReleaseAddressInput{AllocationId: ip.AllocationId})
			if err != nil {
				if ip.AssociationId != nil {
					if s.disassociateAddress(ip) != nil {
						return false, err
					}
				}
				return false, err
			}
			return true, nil
		}, awserrors.AuthFailure, awserrors.InUseIPAddress); err != nil {
			record.Warnf(s.scope.InfraCluster(), "FailedReleaseEIP", "Failed to disassociate Elastic IP %q: %v", *ip.AllocationId, err)
			return errors.Wrapf(err, "failed to release ElasticIP %q", *ip.AllocationId)
		}

		s.scope.Info("released ElasticIP", "eip", *ip.PublicIp, "allocation-id", *ip.AllocationId)
	}
	return nil
}

func (s *Service) getEIPTagParams(role string) infrav1.BuildParams {
	name := fmt.Sprintf("%s-eip-%s", s.scope.Name(), role)

	return infrav1.BuildParams{
		ClusterName: s.scope.Name(),
		Lifecycle:   infrav1.ResourceLifecycleOwned,
		Name:        aws.String(name),
		Role:        aws.String(role),
		Additional:  s.scope.AdditionalTags(),
	}
}

func (s *Service) GetAndAssociateAddressesToInstance(num int, role string, instance string) (err error) {
	eips, err := s.getOrAllocateAddresses(num, role)
	if err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedAssociateEIP", "Failed to get Elastic IP for %q: %v", role, err)
		return err
	}
	_, err = s.EC2Client.AssociateAddressWithContext(context.TODO(), &ec2.AssociateAddressInput{
		InstanceId:   aws.String(instance),
		AllocationId: aws.String(eips[0]),
	})
	if err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedAssociateEIP", "Failed to allocate Elastic IP for %q: %v", role, err)
		return errors.Wrapf(err, "failed to associate Elastic IP %s to instance %s", eips[0], instance)
	}
	return nil

}

func (s *Service) GetOrAllocateAddresses(num int, role string) (eips []string, err error) {
	return s.getOrAllocateAddresses(num, role)
}

func (s *Service) publicIpv4PoolHasFreeIPs(want int64) (bool, error) {
	pools, err := s.EC2Client.DescribePublicIpv4Pools(&ec2.DescribePublicIpv4PoolsInput{
		PoolIds: []*string{s.scope.VPC().PublicIpv4Pool},
	})
	if err != nil {
		return false, errors.Wrapf(err, "failed to describe elastic IPs %q", err)
	}
	if len(pools.PublicIpv4Pools) == 0 || len(pools.PublicIpv4Pools) > 1 {
		return false, errors.Wrapf(err, "unexpected number of Public IPv4 Pools. Want 1, got %d", len(pools.PublicIpv4Pools))
	}

	if aws.Int64Value(pools.PublicIpv4Pools[0].TotalAvailableAddressCount) < want {
		return false, nil
	}
	s.scope.Debug("public IPv4 pool has %d IPs available", "eip", aws.Int64Value(pools.PublicIpv4Pools[0].TotalAvailableAddressCount))
	return true, nil
}
