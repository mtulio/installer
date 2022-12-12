package defaults

import (
	"testing"

	"github.com/openshift/installer/pkg/types"
	"github.com/stretchr/testify/assert"
)

func TestInstanceTypes(t *testing.T) {
	type testCase struct {
		name         string
		region       string
		architecture types.Architecture
		expected     []string
		assert       func(*testCase)
	}
	cases := []testCase{
		{
			name:     "default instance types for AMD64",
			expected: []string{"m6i.xlarge", "m5.xlarge", "c5d.2xlarge"},
			assert: func(tc *testCase) {
				instances := InstanceTypes(tc.region, tc.architecture)
				assert.Equal(t, tc.expected, instances, "unexepcted instance type for AMD64")
			},
		}, {
			name:         "default instance types for ARM64",
			architecture: types.ArchitectureARM64,
			expected:     []string{"m6g.xlarge"},
			assert: func(tc *testCase) {
				instances := InstanceTypes(tc.region, tc.architecture)
				assert.Equal(t, tc.expected, instances, "unexepcted instance type for ARM64")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.assert(&tc)
		})
	}
}
