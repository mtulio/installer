package ec2

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"k8s.io/utils/ptr"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/record"
)

const (
	// eipRoleCustomEC2 is the EIP role used when BYO EIP.
	eipRoleCustomEC2 = "ec2-custom"
)

func getElasticIPRoleName(instanceID string) string {
	return fmt.Sprintf("ec2-%s", instanceID)
}

// reconcileElasticIPFromPublicPool reconciles the elastic IP from a custom Public IPv4 Pool.
func (s *Service) reconcileElasticIPFromPublicPool(pool *infrav1.ElasticIPPool, instance *infrav1.Instance) (bool, error) {
	if pool == nil {
		return false, fmt.Errorf("invalid ElasticIPPool configuration: %v", pool)
	}
	shouldRequeue := true
	iip := ptr.Deref(instance.PublicIP, "")
	s.scope.Debug("Reconciling machine with custom Public IPv4 Pool", "instance-id", instance.ID, "instance-state", instance.State, "instance-public-ip", iip, "publicIpv4PoolID", pool.PublicIpv4Pool)

	// Requeue when the instance is not ready to be associated.
	if instance.State != infrav1.InstanceStateRunning {
		s.scope.Debug("Unable to reconcile Elastic IP Pool for instance", "instance-id", instance.ID, "instance-state", instance.State)
		return shouldRequeue, nil
	}

	// All done, must reconcile only when the instance is in running state.
	shouldRequeue = false

	// Prevent running association every reconciliation when it is already done.
	addrs, err := s.netService.GetAddresses(getElasticIPRoleName(instance.ID))
	if err != nil {
		s.scope.Error(err, "error checking if addresses exists for Elastic IP Pool to machine", "eip-role", getElasticIPRoleName(instance.ID))
		return shouldRequeue, err
	}
	if len(addrs.Addresses) > 0 {
		if len(addrs.Addresses) != 1 {
			return shouldRequeue, fmt.Errorf("unexpected number of EIPs allocated to the role. expected 1, got %d", len(addrs.Addresses))
		}
		addr := addrs.Addresses[0]
		// address is already associated.
		if addr.AssociationId != nil && addr.InstanceId != nil && *addr.InstanceId == instance.ID {
			s.scope.Debug("Machine is already associated with an Elastic IP with custom Public IPv4 pool", "eip", addr.AllocationId, "eip-address", addr.PublicIp, "eip-associationID", addr.AssociationId, "eip-instance", addr.InstanceId)
			return shouldRequeue, nil
		}
	}

	// Associate EIP.
	if err := s.getAndAssociateAddressesToInstance(pool, getElasticIPRoleName(instance.ID), instance.ID); err != nil {
		return shouldRequeue, fmt.Errorf("failed to reconcile Elastic IP: %w", err)
	}
	return shouldRequeue, nil
}

// ReleaseElasticIP releases a specific Elastic IP based on the instance role.
func (s *Service) ReleaseElasticIP(instanceID string) error {
	return s.netService.ReleaseAddressByRole(getElasticIPRoleName(instanceID))
}

// getAndAssociateAddressesToInstance find or create an EIP from an instance and role.
func (s *Service) getAndAssociateAddressesToInstance(pool *infrav1.ElasticIPPool, role string, instance string) (err error) {
	eips, err := s.netService.GetOrAllocateAddresses(pool, 1, role)
	if err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedAllocateEIP", "Failed to retrieve Elastic IP for role %q: %v", role, err)
		return err
	}
	if len(eips) == 0 {
		errMsg := fmt.Sprintf("no Elastic IP found for role %q. Want 1, got %d", role, len(eips))
		record.Warnf(s.scope.InfraCluster(), "FailedAllocateEIP", "Failed to retrieve Elastic IP for role %q", role, err)
		return fmt.Errorf(errMsg)
	}
	_, err = s.EC2Client.AssociateAddressWithContext(context.TODO(), &ec2.AssociateAddressInput{
		InstanceId:   aws.String(instance),
		AllocationId: aws.String(eips[0]),
	})
	if err != nil {
		record.Warnf(s.scope.InfraCluster(), "FailedAssociateEIP", "Failed to associate Elastic IP for role %q: %v", role, err)
		return fmt.Errorf("failed to associate Elastic IP %q to instance %q: %w", eips[0], instance, err)
	}
	return nil
}

// ReconcileElasticIP reconciles the elastic IP for a given instance.
func (s *Service) ReconcileElasticIP(pool *infrav1.ElasticIPPool, instance *infrav1.Instance) (bool, error) {
	// BYO Public IPv4 Pool has precendece over BYO EIP.
	if pool != nil {
		return s.reconcileElasticIPFromPublicPool(pool, instance)
	}

	// Check if there are EIPs allocated and unassociated to the role.
	shouldRequeue := true
	addrs, err := s.netService.GetAddresses(eipRoleCustomEC2)
	if err != nil {
		s.scope.Error(err, "error checking if addresses exists for Elastic IP to machine", "eip-role", eipRoleCustomEC2)
		return shouldRequeue, err
	}

	// No BYO EIPs allocated, and no Public IPv4 Pool defined. Use default.
	if len(addrs.Addresses) == 0 {
		shouldRequeue = false
		return shouldRequeue, nil
	}

	unassociatedAddresses := []*ec2.Address{}
	for _, addr := range addrs.Addresses {
		if addr.AssociationId == nil || addr.InstanceId == nil {
			unassociatedAddresses = append(unassociatedAddresses, addr)
		}
		// When BYO EIP already associated to the instance, skip reconciliation.
		if addr.InstanceId != nil && *addr.InstanceId == instance.ID {
			shouldRequeue = false
			s.scope.Debug("Machine is already associated with an Elastic IP", "instance-id", instance.ID)
			return shouldRequeue, nil
		}
	}

	// No BYO EIPs allocated to the role.
	if len(unassociatedAddresses) == 0 {
		shouldRequeue = false
		s.scope.Debug("Skipping BYO EIP association to instance with no free addresses matching the role.", "instance-id", instance.ID, "eip-role", eipRoleCustomEC2)
		return shouldRequeue, nil
	}

	// Use BYO EIPs allocated to the role.
	// Requeue when the instance is not ready to be associated.
	if instance.State != infrav1.InstanceStateRunning {
		s.scope.Debug("Unable to reconcile Elastic IP for instance", "instance-id", instance.ID, "instance-state", instance.State)
		return shouldRequeue, nil
	}

	// All done, must reconcile only when the instance is in running state.
	shouldRequeue = false

	// Associate EIP.
	if err := s.getAndAssociateAddressesToInstance(nil, eipRoleCustomEC2, instance.ID); err != nil {
		return shouldRequeue, fmt.Errorf("failed to reconcile Elastic IP: %w", err)
	}
	return shouldRequeue, nil
}

// hasBYOPublicIP check if there is BYO IP configuration.
func (s *Service) hasBYOPublicIP(scope *scope.MachineScope) bool {
	s.scope.Debug("BYO IP Check 0", "machine", scope.AWSMachine.ObjectMeta.Name)
	// Check if there is BYO Public IPv4 Pool configuration.
	if scope.AWSMachine.Spec.ElasticIPPool != nil && scope.AWSMachine.Spec.ElasticIPPool.PublicIpv4Pool != nil {
		return true
	}

	s.scope.Debug("BYO IP Check 1", "machine", scope.AWSMachine.ObjectMeta.Name)
	// Check if there is BYO EIP allocation.
	addrs, err := s.netService.GetAddresses(eipRoleCustomEC2)
	if err != nil {
		s.scope.Error(err, "error checking if addresses exists for Elastic IP to machine", "eip-role", eipRoleCustomEC2)
		return false
	}

	s.scope.Debug("BYO IP Check 2", "machine", scope.AWSMachine.ObjectMeta.Name)
	// No BYO EIPs allocated, and no Public IPv4 Pool defined. Use default.
	if len(addrs.Addresses) == 0 {
		return false
	}

	freeAddresses := []*ec2.Address{}
	for _, addr := range addrs.Addresses {
		if addr.AssociationId == nil || addr.InstanceId == nil {
			freeAddresses = append(freeAddresses, addr)
		}
	}

	s.scope.Debug("BYO IP Check 3", "machine", scope.AWSMachine.ObjectMeta.Name, "addresses", len(addrs.Addresses), "free-addresses", len(freeAddresses))
	// No BYO EIPs allocated to the role.
	if len(freeAddresses) == 0 {
		s.scope.Debug("Skipping BYO EIP association to instance with no free addresses matching the role.", "eip-role", eipRoleCustomEC2)
		return false
	}

	s.scope.Debug("BYO IP Check 4")
	s.scope.Debug("Found free EIP allocation matching to the instance role, using it.", "eip-role", eipRoleCustomEC2)
	return true
}
