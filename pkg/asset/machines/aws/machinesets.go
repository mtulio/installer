// Package aws generates Machine objects for aws.
package aws

import (
	"fmt"

	machineapi "github.com/openshift/api/machine/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/aws"
	"github.com/pkg/errors"
)

// MachineSets returns a list of machinesets for a machinepool.
func MachineSets(clusterID string, region string, subnets map[string]string, pool *types.MachinePool, role, userDataSecret string, userTags map[string]string) ([]*machineapi.MachineSet, error) {
	if poolPlatform := pool.Platform.Name(); poolPlatform != aws.Name {
		return nil, fmt.Errorf("non-AWS machine-pool: %q", poolPlatform)
	}
	mpool := pool.Platform.AWS
	azs := mpool.Zones

	total := int64(0)
	if pool.Replicas != nil {
		total = *pool.Replicas
	}
	numOfAZs := int64(len(azs))
	var machinesets []*machineapi.MachineSet
	for idx, az := range mpool.Zones {
		replicas := int32(total / numOfAZs)
		if int64(idx) < total%numOfAZs {
			replicas++
		}
		privateSubnet := true
		if pool.Name == types.MachinePoolEdgeRoleName {
			// FIXME Should check field from machinepool spec, like pool.Public, or from AZ Attribute
			// TODO decide if we'll allow deploying nodes in Public Subnets when running in Local Zones (topology)
			privateSubnet = false
		}
		subnet, ok := subnets[az]
		if len(subnets) > 0 && !ok {
			return nil, errors.Errorf("no subnet for zone %s", az)
		}
		machineProviderInput := machineProviderInput{
			clusterID:      clusterID,
			region:         region,
			subnet:         subnet,
			instanceType:   mpool.InstanceType,
			osImage:        mpool.AMIID,
			zone:           az,
			role:           role,
			userDataSecret: userDataSecret,
			root:           &mpool.EC2RootVolume,
			imds:           mpool.EC2Metadata,
			userTags:       userTags,
			privateSubnet:  privateSubnet,
		}
		provider, err := provider(&machineProviderInput)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create provider")
		}
		name := fmt.Sprintf("%s-%s-%s", clusterID, pool.Name, az)
		spec := machineapi.MachineSpec{
			ProviderSpec: machineapi.ProviderSpec{
				Value: &runtime.RawExtension{Object: provider},
			},
		}
		if pool.Name == types.MachinePoolEdgeRoleName {
			spec.ObjectMeta = machineapi.ObjectMeta{
				Labels: map[string]string{
					"node-role.kubernetes.io/edge": "",
				},
			}
			spec.Taints = []corev1.Taint{
				{
					Key:    "node-role.kubernetes.io/edge",
					Effect: "NoSchedule",
				},
			}
		}
		mset := &machineapi.MachineSet{
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
		machinesets = append(machinesets, mset)
	}

	return machinesets, nil
}
