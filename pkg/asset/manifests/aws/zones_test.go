package aws

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	capa "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"

	"github.com/openshift/installer/pkg/asset/installconfig"
	"github.com/openshift/installer/pkg/asset/installconfig/aws"
	"github.com/openshift/installer/pkg/asset/manifests/capiutils"
	"github.com/openshift/installer/pkg/ipnet"
	"github.com/openshift/installer/pkg/types"
	awstypes "github.com/openshift/installer/pkg/types/aws"
	"github.com/stretchr/testify/assert"
)

var (
	stubDefaultCIDR = "10.0.0.0/16"
)

func stubClusterID() *installconfig.ClusterID {
	return &installconfig.ClusterID{
		InfraID: "infra-id",
	}
}

func stubInstallConfig() *installconfig.InstallConfig {
	ic := &installconfig.InstallConfig{}
	return ic
}

func stubInstallConfigComplete() *installconfig.InstallConfig {
	ic := stubInstallConfig()
	ic.Config = stubInstallConfigType()
	ic.Config.Compute = stubInstallCOnfigPoolCompute()
	ic.Config.ControlPlane = stubInstallCOnfigPoolControl()
	return ic
}

func stubInstallConfigType() *types.InstallConfig {
	return &types.InstallConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: types.InstallConfigVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cluster",
		},
		Publish: types.ExternalPublishingStrategy,
		Networking: &types.Networking{
			MachineNetwork: []types.MachineNetworkEntry{
				{
					CIDR: *ipnet.MustParseCIDR(stubDefaultCIDR),
				},
			},
		},
	}
}
func stubInstallConfigTypeZones() *types.InstallConfig {
	c := &types.InstallConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: types.InstallConfigVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cluster",
		},
		Publish: types.ExternalPublishingStrategy,
		Networking: &types.Networking{
			MachineNetwork: []types.MachineNetworkEntry{
				{
					CIDR: *ipnet.MustParseCIDR(stubDefaultCIDR),
				},
			},
		},
	}
	c.ControlPlane = stubInstallCOnfigPoolControl()
	c.Compute = stubInstallCOnfigPoolCompute()
	return c
}

func stubInstallCOnfigPoolCompute() []types.MachinePool {
	return []types.MachinePool{
		{
			Name: "worker",
			Platform: types.MachinePoolPlatform{
				AWS: &awstypes.MachinePool{
					Zones: []string{"b", "c"},
				},
			},
		},
	}
}

func stubInstallCOnfigPoolControl() *types.MachinePool {
	return &types.MachinePool{
		Name: "master",
		Platform: types.MachinePoolPlatform{
			AWS: &awstypes.MachinePool{
				Zones: []string{"a", "b"},
			},
		},
	}
}

func Test_extractZonesFromInstallConfig(t *testing.T) {
	type args struct {
		in *zoneConfigInput
	}
	tests := []struct {
		name    string
		args    args
		want    []*aws.Zone
		wantErr bool
	}{
		{
			name: "empty install config",
			args: args{
				in: &zoneConfigInput{
					Config: nil,
				},
			},
			wantErr: true,
		},
		{
			name: "default zones",
			args: args{
				in: &zoneConfigInput{
					Config: stubInstallConfigType(),
				},
			},
			want: nil,
		},
		{
			name: "custom zones control plane pool",
			args: args{
				in: &zoneConfigInput{
					Config: func() *types.InstallConfig {
						config := types.InstallConfig{
							ControlPlane: stubInstallCOnfigPoolControl(),
							Compute:      nil,
						}
						return &config
					}(),
				},
			},
			want: []*aws.Zone{{Name: "a"}, {Name: "b"}},
		},
		{
			name: "custom zones compute pool",
			args: args{
				in: &zoneConfigInput{
					Config: func() *types.InstallConfig {
						config := types.InstallConfig{
							ControlPlane: nil,
							Compute:      stubInstallCOnfigPoolCompute(),
						}
						return &config
					}(),
				},
			},
			want: []*aws.Zone{{Name: "b"}, {Name: "c"}},
		},
		{
			name: "custom zones control plane and compute pools",
			args: args{
				in: &zoneConfigInput{
					Config: func() *types.InstallConfig {
						c := &types.InstallConfig{}
						c.ControlPlane = stubInstallCOnfigPoolControl()
						c.Compute = stubInstallCOnfigPoolCompute()
						return c
					}(),
				},
			},
			want: []*aws.Zone{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractZonesFromInstallConfig(tt.args.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractZonesFromInstallConfig() error: %v, wantErr: %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractZonesFromInstallConfig() = got: \n%+v,\nwant: %+v\n", got, tt.want)
			}
		})
	}
}

var stubAwsCluster = &capa.AWSCluster{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "infraId",
		Namespace: capiutils.Namespace,
	},
	Spec: capa.AWSClusterSpec{},
}

func Test_setZonesManagedVPC(t *testing.T) {
	type args struct {
		in *zoneConfigInput
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		want    *capa.AWSCluster
	}{
		{
			name: "empty",
			args: args{
				in: &zoneConfigInput{},
			},
			wantErr: true,
		},
		{
			name: "regular zones",
			args: args{
				in: &zoneConfigInput{
					ClusterID:     stubClusterID(),
					InstallConfig: stubInstallConfigComplete(),
					Config:        stubInstallConfigTypeZones(),
					Cluster:       stubAwsCluster,
				},
			},
			want: func() *capa.AWSCluster {
				c := capa.AWSCluster{}
				c.Spec.NetworkSpec.VPC = capa.VPCSpec{CidrBlock: stubDefaultCIDR}
				c.Spec.NetworkSpec.Subnets = append(c.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
					ID:               "infra-id-subnet-private-a",
					AvailabilityZone: "a",
					IsPublic:         false,
					CidrBlock:        "10.0.0.0/19",
				})
				c.Spec.NetworkSpec.Subnets = append(c.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
					ID:               "infra-id-subnet-public-a",
					AvailabilityZone: "a",
					IsPublic:         true,
					CidrBlock:        "10.0.96.0/21",
				})
				c.Spec.NetworkSpec.Subnets = append(c.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
					ID:               "infra-id-subnet-private-b",
					AvailabilityZone: "b",
					IsPublic:         false,
					CidrBlock:        "10.0.32.0/19",
				})
				c.Spec.NetworkSpec.Subnets = append(c.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
					ID:               "infra-id-subnet-public-b",
					AvailabilityZone: "b",
					IsPublic:         true,
					CidrBlock:        "10.0.104.0/21",
				})
				c.Spec.NetworkSpec.Subnets = append(c.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
					ID:               "infra-id-subnet-private-c",
					AvailabilityZone: "c",
					IsPublic:         false,
					CidrBlock:        "10.0.64.0/19",
				})
				c.Spec.NetworkSpec.Subnets = append(c.Spec.NetworkSpec.Subnets, capa.SubnetSpec{
					ID:               "infra-id-subnet-public-c",
					AvailabilityZone: "c",
					IsPublic:         true,
					CidrBlock:        "10.0.112.0/21",
				})
				return &c
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := setZonesManagedVPC(tt.args.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("setZonesManagedVPC() error: %+v,\nwantErr %+v\n", err, tt.wantErr)
			}
			var got *capa.AWSCluster
			if tt.args.in != nil && tt.args.in.Cluster != nil {
				got = tt.args.in.Cluster
			}
			if tt.want != nil {
				assert.EqualValuesf(t, tt.want.Spec.NetworkSpec, got.Spec.NetworkSpec, "%v failed", tt.name)
			}

		})
	}
}
