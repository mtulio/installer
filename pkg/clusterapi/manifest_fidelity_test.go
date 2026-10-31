package clusterapi

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// droppedPaths returns every leaf path present in want that is absent from
// got, or present with a different value. Extra fields in got are ignored:
// decoding legitimately adds zero values and defaults, and only loss matters.
func droppedPaths(want, got interface{}, path string) []string {
	var out []string
	switch w := want.(type) {
	case map[string]interface{}:
		g, ok := got.(map[string]interface{})
		if !ok {
			return []string{fmt.Sprintf("%s (object became %T)", path, got)}
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, present := g[k]
			if !present {
				out = append(out, path+"."+k)
				continue
			}
			out = append(out, droppedPaths(w[k], gv, path+"."+k)...)
		}
	case []interface{}:
		g, ok := got.([]interface{})
		if !ok {
			return []string{fmt.Sprintf("%s (list became %T)", path, got)}
		}
		if len(g) != len(w) {
			return []string{fmt.Sprintf("%s (list length %d became %d)", path, len(w), len(g))}
		}
		for i := range w {
			out = append(out, droppedPaths(w[i], g[i], fmt.Sprintf("%s[%d]", path, i))...)
		}
	default:
		if fmt.Sprintf("%v", want) != fmt.Sprintf("%v", got) {
			out = append(out, fmt.Sprintf("%s (%v became %v)", path, want, got))
		}
	}
	return out
}

// TestObjectsFromManifestDoesNotDropFields guards a silent-failure mode that
// is specific to a user-supplied manifest.
//
// When a manifest's kind happens to be compiled into this binary, the document
// is converted into the vendored Go type rather than carried through as
// unstructured. Any field the CRD accepts but the vendored struct does not
// have is then dropped here, before the object is ever sent, with no error and
// nothing in the log. The user's manifest is silently not the object that gets
// created, and the provider applies its own defaults instead -- which is how a
// manifest asking for two network load balancers can produce one classic one.
//
// The manifest below is the pilot's, which mirrors what the installer
// generates for the integrated AWS platform.
func TestObjectsFromManifestDoesNotDropFields(t *testing.T) {
	manifest := []byte(`
apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: test-cluster
  namespace: openshift-cluster-api-guests
spec:
  clusterNetwork:
    apiServerPort: 6443
  controlPlaneEndpoint:
    host: ""
    port: 0
  infrastructureRef:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
    kind: AWSCluster
    name: test-cluster
    namespace: openshift-cluster-api-guests
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: AWSCluster
metadata:
  name: test-cluster
  namespace: openshift-cluster-api-guests
spec:
  additionalTags:
    kubernetes.io/cluster/test-cluster: owned
  bastion:
    enabled: false
  controlPlaneEndpoint:
    host: ""
    port: 0
  controlPlaneLoadBalancer:
    additionalListeners:
    - healthCheck:
        intervalSeconds: 10
        path: /healthz
        port: "22623"
        protocol: HTTPS
        thresholdCount: 2
        timeoutSeconds: 10
        unhealthyThresholdCount: 2
      port: 22623
      protocol: TCP
      targetGroupIPType: ipv4
    crossZoneLoadBalancing: true
    healthCheck:
      intervalSeconds: 10
      thresholdCount: 2
      timeoutSeconds: 10
      unhealthyThresholdCount: 2
    healthCheckProtocol: HTTPS
    ingressRules:
    - description: Machine Config Server internal traffic from cluster
      fromPort: 22623
      protocol: tcp
      sourceSecurityGroupRoles:
      - node
      - controlplane
      toPort: 22623
    loadBalancerType: nlb
    name: test-cluster-int
    scheme: internal
    targetGroupIPType: ipv4
  network:
    additionalControlPlaneIngressRules:
    - description: MCS traffic from cluster network
      fromPort: 22623
      protocol: tcp
      sourceSecurityGroupRoles:
      - node
      - controlplane
      - apiserver-lb
      toPort: 22623
    cni:
      cniIngressRules:
      - description: ICMP
        fromPort: -1
        protocol: icmp
        toPort: -1
    nodePortIngressRuleCidrBlocks:
    - 10.0.0.0/16
    subnets:
    - availabilityZone: us-east-1a
      cidrBlock: 10.0.0.0/19
      id: test-cluster-subnet-private-us-east-1a
      isPublic: false
    - availabilityZone: us-east-1a
      cidrBlock: 10.0.160.0/22
      id: test-cluster-subnet-public-us-east-1a
      isPublic: true
    vpc:
      cidrBlock: 10.0.0.0/16
  region: us-east-1
  secondaryControlPlaneLoadBalancer:
    crossZoneLoadBalancing: true
    healthCheckProtocol: HTTPS
    ingressRules:
    - cidrBlocks:
      - 0.0.0.0/0
      description: Kubernetes API Server traffic for public access
      fromPort: 6443
      protocol: tcp
      toPort: 6443
    loadBalancerType: nlb
    name: test-cluster-ext
    scheme: internet-facing
    targetGroupIPType: ipv4
`)

	decoded, err := ObjectsFromManifest("cluster.yaml", manifest)
	require.NoError(t, err)
	require.Len(t, decoded, 2)

	for _, d := range decoded {
		var want map[string]interface{}
		require.NoError(t, yaml.Unmarshal(d.Data, &want))

		rt, err := yaml.Marshal(d.Object)
		require.NoError(t, err)
		var got map[string]interface{}
		require.NoError(t, yaml.Unmarshal(rt, &got))

		kind, _ := want["kind"].(string)
		// TypeMeta is cleared by conversion and restored at write time; it is
		// covered separately and is not what this test is about.
		delete(want, "apiVersion")
		delete(want, "kind")

		assert.Empty(t, droppedPaths(want, got, kind),
			"%s: the manifest is not the object that would be created", kind)
	}
}

// A second test here used to run the same fidelity check against the example
// manifests shipped in this repository, by globbing
// upi/external/examples/*/external-install/cluster.yaml.
//
// Those examples are no longer in this repository: commit 17ba290f47 moved
// them to openshift-splat-team/installer-labs, under external-capi/examples/.
// A test in the installer cannot glob another repository's working tree
// without reintroducing exactly the defect it was written to remove -- it
// originally read a sibling workspace directory and skipped when absent,
// so it never ran anywhere but one laptop.
//
// TestObjectsFromManifestDoesNotDropFields above covers the decode path
// itself with inline fixtures, which is the part that lives here. Checking
// the published examples against it belongs to whatever gates
// installer-labs, not to this package.
