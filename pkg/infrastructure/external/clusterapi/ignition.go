package clusterapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/coreos/ignition/v2/config/v3_2"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"

	infracapi "github.com/openshift/installer/pkg/infrastructure/clusterapi"
	"github.com/openshift/installer/pkg/infrastructure/external/hooks"
	externaltypes "github.com/openshift/installer/pkg/types/external"
)

// Ignition publishes the ignition configs as Cluster API bootstrap Secrets,
// one per machine role.
//
// The shared path already does this for providers that do not implement
// IgnitionProvider, but it creates only two Secrets -- `<infraID>-bootstrap`
// and `<infraID>-master` (pkg/infrastructure/clusterapi/clusterapi.go:367-369).
// That is the right default for an integrated platform, where workers are not
// created by Cluster API at all: they arrive after bootstrap, from machine-api
// MachineSets reconciled by an in-cluster actuator for that cloud.
//
// A non-integrated provider ships no machine-api actuator, so on this platform
// nothing ever creates a worker. Without a third Secret the cluster is capped
// at its control plane forever, because a worker Machine's
// spec.bootstrap.dataSecretName would have nothing valid to point at. Pointing
// it at `<infraID>-master` is not a workaround -- it would boot the node with
// the master pointer config and it would join as a control-plane node.
//
// So External implements this interface to add `<infraID>-worker`, and to run
// the ignition hook. Doing either here rather than in the shared `else`
// branch is deliberate: that branch is reached by every platform that does not
// override it, and changing it would change what those platforms create.
//
// WorkerIgnData is the pointer config -- it fetches the real config from the
// machine config server at api-int:22623/config/worker -- so the Secret is
// small, and it is served by the bootstrap node's MCS until the permanent one
// takes over. A worker created before bootstrap completes therefore retries
// until the MCS answers, rather than failing.
//
// The Secret is created whether or not any worker Machine references it. It is
// inert if unused, it never reaches the cluster being installed, and creating
// it unconditionally means the user can add a worker manifest without also
// having to know that a flag elsewhere controls whether its bootstrap data
// exists.
//
// "Local" is not "in memory". The local control plane's etcd database is a
// directory in the install directory -- pkg/clusterapi/localcontrolplane.go:77
// points envtest at <install-dir>/.clusterapi_output/etcd -- so every Secret
// here is day-0 material at rest on the installer host for the duration of the
// install. It is removed by CleanEtcd (pkg/clusterapi/system.go:571), which
// runs when provisioning fails, when the bootstrap is destroyed and on
// `destroy cluster`, but not if the process is killed before its defers run.
func (p Provider) Ignition(ctx context.Context, in infracapi.IgnitionInput) ([]*corev1.Secret, error) {
	bootstrapIgnData, err := stageBootstrapIgnition(ctx, in)
	if err != nil {
		return nil, err
	}

	secrets := []*corev1.Secret{
		infracapi.IgnitionSecret(bootstrapIgnData, in.InfraID, "bootstrap"),
		infracapi.IgnitionSecret(in.MasterIgnData, in.InfraID, "master"),
	}

	// Defensive: the worker ignition asset is generated for every platform
	// (pkg/asset/targets/targets.go:62,71), so this is expected to be set.
	// An empty Secret would be worse than an absent one, because a Machine
	// referencing it would provision and then fail to boot with nothing
	// pointing at the cause.
	if len(in.WorkerIgnData) > 0 {
		secrets = append(secrets, infracapi.IgnitionSecret(in.WorkerIgnData, in.InfraID, "worker"))
	}

	return secrets, nil
}

// stageBootstrapIgnition runs the payload's ignition hook, if it declares
// one, and returns the bytes to put in the bootstrap Secret.
//
// Why the installer reads a file back, when no other hook returns anything:
// because the installer owns the Secret the provider reads from. A Machine
// takes its bootstrap data through spec.bootstrap.dataSecretName, and core
// Cluster API is explicit that a Machine without one "should remain in the
// Pending state" (vendor/sigs.k8s.io/cluster-api/api/core/v1beta1/
// machine_types.go:668-673). The reference payload's bootstrap Machine names
// `${INSTALLER_INFRA_ID}-bootstrap`, which only this function creates. A hook
// that staged bytes and kept them to itself would have no route to the
// provider at all.
//
// CAPA is not a counter-example. AWSCluster.spec.s3Bucket acts downstream of a
// read that already happened -- cluster-api-provider-aws/pkg/cloud/scope/
// machine.go:275-300 takes secret.Data["value"] first -- so it is an extra
// exit, not a second entrance.
//
// Returned unchanged when the payload declares no hook, which is the ordinary
// case: a provider whose cloud can carry a full ignition in instance metadata
// needs none of this.
func stageBootstrapIgnition(ctx context.Context, in infracapi.IgnitionInput) ([]byte, error) {
	declared := configuredHooks(in.InstallConfig)
	if declared == nil || declared.Ignition == nil {
		return in.BootstrapIgnData, nil
	}
	hook := declared.Ignition

	// A hook was resolved, so a payload was resolved, so this is the External
	// platform. Checked anyway: the alternative to an error here is a nil
	// dereference inside a branch that only one provider ever reaches.
	if in.InstallConfig == nil || in.InstallConfig.Config == nil ||
		in.InstallConfig.Config.Platform.External == nil ||
		in.InstallConfig.Config.Platform.External.ClusterAPI == nil {
		return nil, fmt.Errorf("the provider payload declares an ignition hook but no %s platform "+
			"Cluster API provider is configured", externaltypes.Name)
	}

	// Checked again here, having already been checked at PreProvision. The
	// two are not redundant: PreProvision is where the refusal is cheap, and
	// this one is the gate on the handover itself. Anything that reaches this
	// function without having gone through PreProvision -- a test, a future
	// refactor of the provisioning order -- is still refused rather than
	// handed the cluster's secrets because a check elsewhere was assumed.
	if err := checkIgnitionStagingConsent(in.InstallConfig.Config.Platform.External.ClusterAPI); err != nil {
		return nil, err
	}

	// Two directories, both removed when this returns. The input is kept out
	// of the output directory so that what the hook wrote and what it was
	// given cannot be confused -- including by the hook itself, which would
	// otherwise be able to satisfy its contract by doing nothing.
	inDir, err := os.MkdirTemp("", "openshift-install-ignition-in-")
	if err != nil {
		return nil, fmt.Errorf("failed to create a directory for the bootstrap ignition: %w", err)
	}
	defer os.RemoveAll(inDir)

	outDir, err := os.MkdirTemp("", "openshift-install-ignition-out-")
	if err != nil {
		return nil, fmt.Errorf("failed to create the hook output directory: %w", err)
	}
	defer os.RemoveAll(outDir)

	inPath := filepath.Join(inDir, "bootstrap.ign")
	if err := os.WriteFile(inPath, in.BootstrapIgnData, 0o600); err != nil {
		return nil, fmt.Errorf("failed to write the bootstrap ignition for the hook: %w", err)
	}

	req, err := buildRequest(ctx, in.Client, hooks.Ignition, hook, in.InstallConfig, in.InfraID)
	if err != nil {
		return nil, err
	}
	req.BootstrapIgnitionPath = inPath
	req.OutputDir = outDir
	if err := hooks.Run(ctx, req); err != nil {
		return nil, err
	}

	// Localize again at the point of use. It was checked when the descriptor
	// was loaded, but this string has travelled through a resolution since
	// then and joining an unchecked relative path to a directory is the kind
	// of thing that is only ever checked once by accident.
	local, err := filepath.Localize(hook.BootstrapOutputPath)
	if err != nil {
		return nil, fmt.Errorf("the ignition hook's bootstrapOutputPath %q must be a relative path "+
			"inside the output directory: %w", hook.BootstrapOutputPath, err)
	}
	staged, err := os.ReadFile(filepath.Join(outDir, local))
	if err != nil {
		return nil, fmt.Errorf("the ignition hook exited successfully but did not write the %q it "+
			"declares in the provider payload: %w", hook.BootstrapOutputPath, err)
	}

	// A hook that fails silently and copies its input is the failure worth
	// catching, and it is the one a success check cannot see. Left alone it
	// puts the full ignition back into the Secret, and the install then dies
	// at the cloud API with a metadata-size error naming nothing that happened
	// here. Comparing digests costs nothing and names it exactly.
	inDigest := sha256.Sum256(in.BootstrapIgnData)
	outDigest := sha256.Sum256(staged)
	if inDigest == outDigest {
		return nil, fmt.Errorf("the ignition hook wrote %q with the same contents as the bootstrap "+
			"ignition it was given (sha256:%s): it is expected to replace the ignition with something "+
			"smaller, such as a pointer config, not to copy it",
			hook.BootstrapOutputPath, hex.EncodeToString(inDigest[:]))
	}

	// Parsed, not merely non-empty. The provider hands these bytes to the
	// cloud and the machine fails to boot with nothing to read; a parse here
	// turns that into an error next to the hook that produced it.
	if _, report, err := v3_2.Parse(staged); err != nil {
		return nil, fmt.Errorf("the ignition hook wrote %q, which is not a valid Ignition config: %w (%s)",
			hook.BootstrapOutputPath, err, report.String())
	}

	// Size and digest only. The file is a pointer config in the intended case
	// and the whole bootstrap ignition in the case this function exists to
	// prevent, so it is never something to log the contents of.
	logrus.Infof("The ignition hook replaced the bootstrap ignition: %d bytes (sha256:%s), was %d bytes "+
		"(sha256:%s)", len(staged), hex.EncodeToString(outDigest[:]),
		len(in.BootstrapIgnData), hex.EncodeToString(inDigest[:]))
	return staged, nil
}

// checkIgnitionStagingConsent refuses an ignition hook the cluster creator has
// not consented to.
//
// Consent cannot be checked at install-config validation, because the hook is
// declared by the payload and the payload is not available until it has been
// pulled. It is a refusal rather than a warning: the hook is handed the
// cluster's day-0 secret material, and that is not something to do because a
// payload asked for it.
//
// It is called from two places. PreProvision calls it as soon as the payload
// is resolved, which is before the local control plane starts and before any
// cloud resource exists, so the common mistake -- a payload that declares the
// hook and an install-config that never allowed it -- costs nothing. Ignition
// calls it again at the point the bytes actually change hands.
func checkIgnitionStagingConsent(capi *externaltypes.ClusterAPIProvider) error {
	if capi.AllowsBootstrapIgnitionStaging() {
		return nil
	}
	return fmt.Errorf("the provider payload declares an ignition hook, which is handed the "+
		"cluster's bootstrap ignition and therefore every secret in it; set "+
		"platform.external.clusterAPI.bootstrapIgnitionStaging to %q to allow it",
		externaltypes.BootstrapIgnitionStagingAllowed)
}
