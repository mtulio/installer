package defaults

import (
	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/aws"
)

var (
	defaultMachineTypes = map[types.Architecture]map[string][]string{
		types.ArchitectureAMD64: {
			// Example region default machine class override for AMD64:
			// "ap-east-1":      {"m6i.xlarge", "m5.xlarge"},
		},
		types.ArchitectureARM64: {
			// Example region default machine class override for ARM64:
			// "us-east-1":      {"m6g.xlarge", "m6gd.xlarge"},
		},
	}
)

// SetPlatformDefaults sets the defaults for the platform.
func SetPlatformDefaults(p *aws.Platform) {
}

// InstanceTypes returns a list of instance types, in decreasing priority order, which we should use for a given
// region. Default is m6i.xlarge, m5.xlarge, lastly c5d.2xlarge unless a region override
// is defined in defaultMachineTypes.
// c5d.2xlarge is in the most locations of availability for Local Zone offerings.
// https://aws.amazon.com/about-aws/global-infrastructure/localzones/features
// https://aws.amazon.com/ec2/pricing/on-demand/
func InstanceTypes(region string, arch types.Architecture) []string {
	if classesForArch, ok := defaultMachineTypes[arch]; ok {
		if classes, ok := classesForArch[region]; ok {
			return classes
		}
	}

	switch arch {
	case types.ArchitectureARM64:
		return []string{"m6g.xlarge"}
	default:
		return []string{"m6i.xlarge", "m5.xlarge", "c5d.2xlarge"}
	}
}
