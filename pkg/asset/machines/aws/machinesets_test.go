// Package aws generates Machine objects for aws.
package aws

import (
	"fmt"
	"testing"

	machineapi "github.com/openshift/api/machine/v1beta1"
	icaws "github.com/openshift/installer/pkg/asset/installconfig/aws"
	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/aws"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/pointer"
)

const (
	testClusterID = "clusterID"
)

type MachineSetsInput struct {
	clusterID      string
	region         string
	subnets        *icaws.Subnets
	pool           *types.MachinePool
	role           string
	userDataSecret string
	userTags       map[string]string
}

func validMachineSetInput() *MachineSetsInput {
	input := MachineSetsInput{}
	return &input
}

func validMachineProvider(in *machineProviderInput) *machineapi.AWSMachineProviderConfig {
	tags := []machineapi.TagSpecification{}

	return &machineapi.AWSMachineProviderConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "machine.openshift.io/v1beta1",
			Kind:       "AWSMachineProviderConfig",
		},
		InstanceType: in.instanceType,
		BlockDevices: []machineapi.BlockDeviceMappingSpec{
			{
				EBS: &machineapi.EBSBlockDeviceSpec{
					VolumeType: pointer.StringPtr(in.root.Type),
					VolumeSize: pointer.Int64Ptr(int64(in.root.Size)),
					Iops:       pointer.Int64Ptr(int64(in.root.IOPS)),
					Encrypted:  pointer.BoolPtr(true),
					KMSKey:     machineapi.AWSResourceReference{ARN: pointer.StringPtr(in.root.KMSKeyARN)},
				},
			},
		},
		Tags: tags,
		IAMInstanceProfile: &machineapi.AWSResourceReference{
			ID: pointer.StringPtr(fmt.Sprintf("%s-%s-profile", in.clusterID, in.role)),
		},
		UserDataSecret:    &corev1.LocalObjectReference{Name: in.userDataSecret},
		CredentialsSecret: &corev1.LocalObjectReference{Name: "aws-cloud-credentials"},
		Placement:         machineapi.Placement{Region: in.region, AvailabilityZone: in.zone},
		SecurityGroups: []machineapi.AWSResourceReference{{
			Filters: []machineapi.Filter{{
				Name:   "tag:Name",
				Values: []string{fmt.Sprintf("%s-%s-sg", in.clusterID, in.role)},
			}},
		}},
	}
}

func validMachineProviderCompute(role, region, az string) *machineapi.AWSMachineProviderConfig {
	return validMachineProvider(&machineProviderInput{
		instanceType: "m6i.xlarge",
		root: &aws.EC2RootVolume{
			Type: "gp3",
			Size: 120,
		},
		clusterID:      testClusterID,
		role:           role,
		userDataSecret: "userDataSecret",
		region:         region,
		zone:           az,
	})
}

func validMachineSetByRole(role, region, az string) *machineapi.MachineSet {
	// var results []*machineapi.MachineSet

	clusterID := testClusterID
	name := fmt.Sprintf("%s-%s-%s", clusterID, role, az)
	replicas := int32(1)

	provider := validMachineProviderCompute(role, region, az)
	spec := machineapi.MachineSpec{
		ProviderSpec: machineapi.ProviderSpec{
			Value: &runtime.RawExtension{Object: provider},
		},
		ObjectMeta: machineapi.ObjectMeta{
			Labels: make(map[string]string, 3),
		},
		Taints: []corev1.Taint{},
	}
	result := &machineapi.MachineSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "machine.openshift.io/v1beta1",
			Kind:       "MachineSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "openshift-machine-api",
			Name:      name,
			Labels: map[string]string{
				"machine.openshift.io/cluster-api-cluster": clusterID,
			},
		},
		Spec: machineapi.MachineSetSpec{
			Replicas: &replicas,
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"machine.openshift.io/cluster-api-machineset": name,
					"machine.openshift.io/cluster-api-cluster":    clusterID,
				},
			},
			Template: machineapi.MachineTemplateSpec{
				ObjectMeta: machineapi.ObjectMeta{
					Labels: map[string]string{
						"machine.openshift.io/cluster-api-machineset":   name,
						"machine.openshift.io/cluster-api-cluster":      clusterID,
						"machine.openshift.io/cluster-api-machine-role": role,
						"machine.openshift.io/cluster-api-machine-type": role,
					},
				},
				Spec: spec,
				// we don't need to set Versions, because we control those via cluster operators.
			},
		},
	}
	return result
}

func validMachineSets() []*machineapi.MachineSet {
	machineSets := []*machineapi.MachineSet{}
	machineSets = append(machineSets, validMachineSetByRole("worker", "us-east-1", "us-east-1a"))
	return machineSets
}

func TestMachineSets(t *testing.T) {
	cases := []struct {
		name      string
		input     *MachineSetsInput
		expect    []*machineapi.MachineSet
		expectErr bool
		errMatch  string
		errMsg    string
	}{
		{
			name:      "valid machine pool",
			input:     validMachineSetInput(),
			expect:    validMachineSets(),
			expectErr: false,
		},
		{
			name: "invalid pool",
			input: func() *MachineSetsInput {
				input := MachineSetsInput{}
				return &input
			}(),
			expect: func() []*machineapi.MachineSet {
				var results []*machineapi.MachineSet
				results = append(results, &machineapi.MachineSet{})
				return results
			}(),
			expectErr: true,
			errMatch:  "invalid pool",
		},
	}

	for _, tc := range cases {
		machineSets, err := MachineSets(
			tc.input.clusterID,
			tc.input.region,
			tc.input.subnets,
			tc.input.pool,
			tc.input.role,
			tc.input.userDataSecret,
			tc.input.userTags,
		)
		if tc.expectErr {
			if assert.Error(t, err) {
				assert.Regexp(t, tc.errMatch, err.Error())
			}
		} else {
			assert.Equal(t, machineSets, tc.expect, tc.errMsg)
		}
	}
}
