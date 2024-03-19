package aws

import (
	"context"
	"fmt"
	"net"

	capa "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"

	"github.com/openshift/installer/pkg/asset/installconfig"
	"github.com/openshift/installer/pkg/asset/installconfig/aws"
	"github.com/openshift/installer/pkg/asset/manifests/capiutils"
	utilscidr "github.com/openshift/installer/pkg/asset/manifests/capiutils/cidr"
	"github.com/openshift/installer/pkg/types"
)

type zoneConfigInput struct {
	InstallConfig *installconfig.InstallConfig
	Config        *types.InstallConfig
	Meta          *aws.Metadata
	Cluster       *capa.AWSCluster
	ClusterID     *installconfig.ClusterID
	ZonesInRegion []string
}

func (zin *zoneConfigInput) SetZoneMetadata() (err error) {
	if zin.InstallConfig == nil {
		return fmt.Errorf("failed to get installConfig: %w", err)
	}
	if zin.InstallConfig.AWS == nil {
		return fmt.Errorf("failed to get AWS metadata: %w", err)
	}
	zin.ZonesInRegion, err = zin.InstallConfig.AWS.AvailabilityZones(context.TODO())
	if err != nil {
		return fmt.Errorf("failed to get availability zones: %w", err)
	}
	// QUESTION(mtulio): Should we need to filter the instances available in ZonesInRegion,
	// removing zones which does not match any criteria (instance offering)?
	return nil
}

// setZones creates the CAPI NetworkSpec structures for managed or
// BYO VPC deployments from install-config.yaml.
func setZones(in *zoneConfigInput) error {
	if len(in.Config.AWS.Subnets) > 0 {
		return setZonesBYOVPC(in)
	}

	// TODO : do we need to query the EC2 availability when zone default zones is used?
	err := in.SetZoneMetadata()
	if err != nil {
		return fmt.Errorf("failed to get availability zones from metadata: %w", err)
	}
	return setZonesManagedVPC(in)
}

// setZonesManagedVPC creates the CAPI NetworkSpec.Subnets setting the
// desired subnets from install-config.yaml in the BYO VPC deployment.
func setZonesBYOVPC(in *zoneConfigInput) error {
	privateSubnets, err := in.Meta.PrivateSubnets(context.TODO())
	if err != nil {
		return fmt.Errorf("failed to get private subnets: %w", err)
	}
	for _, subnet := range privateSubnets {
		in.Cluster.Spec.NetworkSpec.Subnets = append(in.Cluster.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
			ID:               subnet.ID,
			CidrBlock:        subnet.CIDR,
			AvailabilityZone: subnet.Zone.Name,
			IsPublic:         subnet.Public,
		})
	}

	publicSubnets, err := in.Meta.PublicSubnets(context.TODO())
	if err != nil {
		return fmt.Errorf("failed to get public subnets: %w", err)
	}
	for _, subnet := range publicSubnets {
		in.Cluster.Spec.NetworkSpec.Subnets = append(in.Cluster.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
			ID:               subnet.ID,
			CidrBlock:        subnet.CIDR,
			AvailabilityZone: subnet.Zone.Name,
			IsPublic:         subnet.Public,
		})
	}

	edgeSubnets, err := in.Meta.EdgeSubnets(context.TODO())
	if err != nil {
		return fmt.Errorf("failed to get edge subnets: %w", err)
	}
	for _, subnet := range edgeSubnets {
		in.Cluster.Spec.NetworkSpec.Subnets = append(in.Cluster.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
			ID:               subnet.ID,
			CidrBlock:        subnet.CIDR,
			AvailabilityZone: subnet.Zone.Name,
			IsPublic:         subnet.Public,
		})
	}

	vpc, err := in.Meta.VPC(context.TODO())
	if err != nil {
		return fmt.Errorf("failed to get VPC: %w", err)
	}
	in.Cluster.Spec.NetworkSpec.VPC = capa.VPCSpec{
		ID: vpc,
	}

	return nil
}

type zonesCAPI struct {
	AvailabilityZones []string
	EdgeZones         []string
}

// setZonesManagedVPC creates the CAPI NetworkSpec.VPC setting the
// desired zones from install-config.yaml in the managed VPC deployment.
func setZonesManagedVPC(in *zoneConfigInput) error {

	out, err := extractZonesFromInstallConfig(in)
	if err != nil {
		return fmt.Errorf("failed to get availability zones: %w", err)
	}

	mainCIDR := capiutils.CIDRFromInstallConfig(in.InstallConfig)
	in.Cluster.Spec.NetworkSpec.VPC = capa.VPCSpec{
		CidrBlock: mainCIDR.String(),
	}

	// Base subnets considering only private zones, leaving one free block to allow
	// future subnet expansions in Day-2.
	numSubnets := len(out.AvailabilityZones) + 1

	// Public subnets consumes one range from base blocks.
	isPublishingExternal := in.Config.Publish == types.ExternalPublishingStrategy
	publicCidrIndex := len(out.AvailabilityZones)
	if isPublishingExternal {
		numSubnets++
	}
	// Edge subnets consumes
	edgeCidrIndex := len(out.AvailabilityZones) + 1
	if len(out.EdgeZones) > 0 {
		numSubnets++
		edgeCidrIndex = len(out.AvailabilityZones) + 1
	}

	privateCIDRs, err := utilscidr.SplitIntoSubnetsIPv4(mainCIDR.String(), numSubnets)
	if err != nil {
		return fmt.Errorf("unable to retrieve CIDR blocks for all private subnets: %w", err)
	}
	var publicCIDRs []*net.IPNet
	if isPublishingExternal {
		publicCIDRs, err = utilscidr.SplitIntoSubnetsIPv4(privateCIDRs[publicCidrIndex].String(), publicCidrIndex)
		if err != nil {
			return fmt.Errorf("unable to retrieve CIDR blocks for all public subnets: %w", err)
		}
	}

	var edgeCIDRs []*net.IPNet
	if len(out.EdgeZones) > 0 {
		numEdgeSubnets := len(out.EdgeZones)
		if isPublishingExternal {
			numEdgeSubnets = numEdgeSubnets * 2
		}
		// Always allowing free blocks at the end of range.
		numEdgeSubnets++
		edgeCIDRs, err = utilscidr.SplitIntoSubnetsIPv4(privateCIDRs[edgeCidrIndex].String(), numEdgeSubnets)
	}

	// Q: Can we use the standard terraform name (without 'subnet') and tell CAPA
	// to query it for Control Planes?
	subnetNamePrefix := fmt.Sprintf("%s-subnet", in.ClusterID.InfraID)

	// Create subnets from zone pool with type availability-zone
	idxCIDR := 0
	for _, zone := range out.AvailabilityZones {
		if len(privateCIDRs) < idxCIDR {
			return fmt.Errorf("unable to define CIDR blocks for all private subnets: %w", err)
		}
		cidr := privateCIDRs[idxCIDR]
		in.Cluster.Spec.NetworkSpec.Subnets = append(in.Cluster.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
			AvailabilityZone: zone,
			CidrBlock:        cidr.String(),
			ID:               fmt.Sprintf("%s-private-%s", subnetNamePrefix, zone),
			IsPublic:         false,
		})
		if isPublishingExternal {
			if len(publicCIDRs) < idxCIDR {
				return fmt.Errorf("unable to define CIDR blocks for all public subnets: %w", err)
			}
			cidr = publicCIDRs[idxCIDR]
			in.Cluster.Spec.NetworkSpec.Subnets = append(in.Cluster.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
				AvailabilityZone: zone,
				CidrBlock:        cidr.String(),
				ID:               fmt.Sprintf("%s-public-%s", subnetNamePrefix, zone),
				IsPublic:         true,
			})
		}
		idxCIDR++
	}

	// Create subnets from zone pool with type local-zone or wavelength-zone (edge zones)
	idxCIDR = 0
	for _, zone := range out.EdgeZones {
		if len(edgeCIDRs) < idxCIDR {
			return fmt.Errorf("unable to define CIDR blocks for all private subnets: %w", err)
		}
		cidr := edgeCIDRs[idxCIDR]
		in.Cluster.Spec.NetworkSpec.Subnets = append(in.Cluster.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
			AvailabilityZone: zone,
			CidrBlock:        cidr.String(),
			ID:               fmt.Sprintf("%s-private-%s", subnetNamePrefix, zone),
			IsPublic:         false,
		})
		idxCIDR++
		if isPublishingExternal {
			if len(edgeCIDRs) < idxCIDR {
				return fmt.Errorf("unable to define CIDR blocks for all public subnets: %w", err)
			}
			cidr = edgeCIDRs[idxCIDR]
			in.Cluster.Spec.NetworkSpec.Subnets = append(in.Cluster.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
				AvailabilityZone: zone,
				CidrBlock:        cidr.String(),
				ID:               fmt.Sprintf("%s-public-%s", subnetNamePrefix, zone),
				IsPublic:         true,
			})
			idxCIDR++
		}
	}

	return nil
}

// extractZonesFromInstallConfig extracts all zones defined in the install-config,
// otherwise discover it based in the AWS metadata when none is defined.
func extractZonesFromInstallConfig(in *zoneConfigInput) (*zonesCAPI, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("unable to retrieve Config")
	}
	out := zonesCAPI{}
	zonesMap := make(map[string]struct{})
	addAvailabilityZones := func(names []string) {
		for _, name := range names {
			if _, ok := zonesMap[name]; !ok {
				zonesMap[name] = struct{}{}
				out.AvailabilityZones = append(out.AvailabilityZones, name)
			}
		}
	}
	addEdgeZones := func(names []string) {
		for _, name := range names {
			if _, ok := zonesMap[name]; !ok {
				zonesMap[name] = struct{}{}
				out.EdgeZones = append(out.EdgeZones, name)
			}
		}
	}

	cfg := in.Config
	defaultZones := []string{}
	if cfg.AWS != nil && cfg.AWS.DefaultMachinePlatform != nil && len(cfg.AWS.DefaultMachinePlatform.Zones) > 0 {
		defaultZones = cfg.AWS.DefaultMachinePlatform.Zones
	}

	if cfg.ControlPlane != nil && cfg.ControlPlane.Platform.AWS != nil &&
		len(cfg.ControlPlane.Platform.AWS.Zones) > 0 {
		addAvailabilityZones(cfg.ControlPlane.Platform.AWS.Zones)
	} else if len(defaultZones) > 0 {
		addAvailabilityZones(defaultZones)
	} else if len(in.ZonesInRegion) > 0 {
		addAvailabilityZones(in.ZonesInRegion)
	}

	for _, compute := range cfg.Compute {
		if compute.Platform.AWS == nil {
			continue
		}
		switch compute.Name {
		case "edge":
			if len(compute.Platform.AWS.Zones) > 0 {
				addEdgeZones(compute.Platform.AWS.Zones)
			}
		default:
			if len(compute.Platform.AWS.Zones) > 0 {
				addAvailabilityZones(compute.Platform.AWS.Zones)
			} else if len(defaultZones) > 0 {
				addAvailabilityZones(defaultZones)
			} else if len(in.ZonesInRegion) > 0 {
				addAvailabilityZones(in.ZonesInRegion)
			}
		}
	}
	return &out, nil
}
