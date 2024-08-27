package elb

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
)

func getElasticIPRoleName() string {
	return fmt.Sprintf("lb-%s", infrav1.APIServerRoleTagValue)
}

func (s *Service) getOrAllocateAddresses(input *elbv2.CreateLoadBalancerInput) error {
	// Only NLB is supported
	if input.Type == nil {
		return fmt.Errorf("PublicIpv4Pool is supported only when the Load Balancer type is %q", elbv2.LoadBalancerTypeEnumNetwork)
	}
	if *input.Type != string(elbv2.LoadBalancerTypeEnumNetwork) {
		return fmt.Errorf("PublicIpv4Pool is not supported with Load Balancer type %s. Use Network Load Balancer instead", *input.Type)
	}

	// Custom SubnetMappings should not be defined or overridden by user-defined mapping.
	if len(input.SubnetMappings) > 0 {
		return fmt.Errorf("PublicIpv4Pool is mutually exclusive with SubnetMappings")
	}

	eips, err := s.netService.GetOrAllocateAddresses(s.scope.VPC().GetElasticIPPool(), len(input.Subnets), getElasticIPRoleName())
	if err != nil {
		return fmt.Errorf("failed to allocate address from Public IPv4 Pool %q to role %s: %w", *s.scope.VPC().GetPublicIpv4Pool(), getElasticIPRoleName(), err)
	}
	if len(eips) != len(input.Subnets) {
		return fmt.Errorf("number of allocated EIP addresses (%d) from pool %q must match with the subnet count (%d)", len(eips), *s.scope.VPC().GetPublicIpv4Pool(), len(input.Subnets))
	}
	for cnt, sb := range input.Subnets {
		input.SubnetMappings = append(input.SubnetMappings, &elbv2.SubnetMapping{
			SubnetId:     aws.String(*sb),
			AllocationId: aws.String(eips[cnt]),
		})
	}
	// Subnets and SubnetMappings are mutual exclusive. Cleaning Subnets when BYO IP is defined,
	// and SubnetMappings are mounted.
	input.Subnets = []*string{}

	return nil
}

// allocatePublicIpv4AddressFromByoIPPool claims for Elastic IPs from an user-defined public IPv4 pool,
// allocating it to the NetworkMapping structure from an Network Load Balancer.
func (s *Service) allocatePublicIpv4AddressFromByoIPPool(input *elbv2.CreateLoadBalancerInput) error {
	// Custom Public IPv4 Pool isn't set.
	if s.scope.VPC().GetPublicIpv4Pool() == nil {
		return nil
	}

	return s.getOrAllocateAddresses(input)
}

// allocatePublicIpv4Address validate if the static IP configuration is defined,
// either by BYO EIP or BYO Public IPv4 Pool, and allocates the IP address to the
// NetworkMapping structure.
// If there is no pre-allocated EIP, and the Public IPv4 Pool is defined, it will
// claim an IP address from the pool.
// If there is no pre-allocated EIP, and the Public IPv4 Pool is not defined, it will
// use the default configuration from the AWS pool.
func (s *Service) allocatePublicIpv4Address(input *elbv2.CreateLoadBalancerInput) error {
	// Custom Public IPv4 Pool is defined.
	if s.scope.VPC().GetPublicIpv4Pool() != nil {
		return s.allocatePublicIpv4AddressFromByoIPPool(input)
	}

	// TODO(mtulio): check if need to implement this in test cases, or just keep the flow without EIP.
	if s.netService == nil {
		return nil
	}

	// Check if there are EIPs allocated and unassociated to the role.
	addrs, err := s.netService.GetAddresses(getElasticIPRoleName())
	if err != nil {
		return fmt.Errorf("failed to check if addresses exists for Elastic IP Pool to Load Balancer: %w", err)
	}

	// No BYO EIPs allocated, and no Public IPv4 Pool defined. Use default.
	if len(addrs.Addresses) == 0 {
		return nil
	}

	// Use BYO EIPs allocated to the role.
	invalidAddresses := []string{}
	if len(addrs.Addresses) > 0 {
		for _, addr := range addrs.Addresses {
			if addr.AssociationId != nil || addr.InstanceId != nil {
				invalidAddresses = append(invalidAddresses, aws.StringValue(addr.AllocationId))
			}
		}
	}
	if len(invalidAddresses) > 0 {
		return fmt.Errorf("one or more BYO EIP addresses are in use, unable to use on Network Load Balancers: %v", invalidAddresses)
	}

	return s.getOrAllocateAddresses(input)
}
