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

package scope

import (
	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// ELBScope is a scope for use with the ELB reconciling service.
type ELBScope interface {
	cloud.ClusterScoper

	// Network returns the cluster network object.
	Network() *infrav1.NetworkStatus

	// Subnets returns the cluster subnets.
	Subnets() infrav1.Subnets

	// SecurityGroups returns the cluster security groups as a map, it creates the map if empty.
	SecurityGroups() map[infrav1.SecurityGroupRole]infrav1.SecurityGroup

	// VPC returns the cluster VPC.
	VPC() *infrav1.VPCSpec

	// ControlPlaneLoadBalancer returns the AWSLoadBalancerSpec
	// Deprecated: Use ControlPlaneLoadBalancers()
	ControlPlaneLoadBalancer() *infrav1.AWSLoadBalancerSpec

	// ControlPlaneLoadBalancerScheme returns the Classic ELB scheme (public or internal facing)
	// Deprecated: This method is going to be removed in a future release. Use LoadBalancer.Scheme.
	ControlPlaneLoadBalancerScheme() infrav1.ELBScheme

	// ControlPlaneLoadBalancerName returns the Classic ELB name
	ControlPlaneLoadBalancerName() *string

	// ControlPlaneEndpoint returns AWSCluster control plane endpoint
	ControlPlaneEndpoint() clusterv1.APIEndpoint

	// ControlPlaneLoadBalancers returns both the ControlPlaneLoadBalancer and SecondaryControlPlaneLoadBalancer AWSLoadBalancerSpecs.
	// The control plane load balancers should always be returned in the above order.
	ControlPlaneLoadBalancers() []*infrav1.AWSLoadBalancerSpec

	// NOTE: The following methods is not implemented, it is defined to satisfy the
	// interface to allow casting scope from network services.
	// Question(mtulio): is there a better method to do this. It is required to consume EIP services
	// from network service. We can isolate EIP service but would generate more refact.

	// Bastion returns the bastion details for the cluster.
	Bastion() *infrav1.Bastion

	// Bucket returns the cluster bucket.
	Bucket() *infrav1.S3Bucket

	// CNIIngressRules returns the CNI spec ingress rules.
	CNIIngressRules() infrav1.CNIIngressRules

	// GetNatGatewaysIPs gets the Nat Gateways Public IPs.
	GetNatGatewaysIPs() []string

	// SecondaryCidrBlock returns the optional secondary CIDR block to use for pod IPs
	SecondaryCidrBlock() *string

	// SetNatGatewaysIPs sets the Nat Gateways Public IPs.
	SetNatGatewaysIPs(ips []string)

	// SetSubnets updates the clusters subnets.
	SetSubnets(subnets infrav1.Subnets)

	// TagUnmanagedNetworkResources returns is tagging unmanaged network resources is set.
	TagUnmanagedNetworkResources() bool
}
