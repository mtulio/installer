// Package hooks runs the user-supplied programs that automate the parts of an
// External install the installer cannot do itself.
//
// The installer knows nothing about a partner's cloud, which is the whole
// premise of provisioning with a user-supplied Cluster API provider. But some
// of what an install needs is not expressible in the Cluster API contract at
// all. DNS is the case that forces this package to exist: a control-plane
// machine fetches its ignition from
// https://api-int.<clusterDomain>:22623/config/master and the bootstrap node's
// kubeconfig targets api-int as well, yet the core Cluster carries one
// spec.controlPlaneEndpoint and has no field for an internal endpoint. An
// integrated platform creates those records in its own InfraReady; this
// platform has to call out to the user instead.
//
// So a hook is the External platform's substitute for provider-specific
// installer code. It runs at the same point in the flow, is handed everything
// the installer can describe without knowing the provider's API, and reports
// failure the same way.
//
// This package is deliberately separate from the provider implementation so
// that `destroy cluster`, which has no install-config and no provider object,
// can run the teardown counterpart through exactly the same code.
package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"

	externaltypes "github.com/openshift/installer/pkg/types/external"
)

// Kind names a point in the install at which a hook runs. The value is passed
// to the program, so one script can serve every hook and dispatch on it --
// which is what the reference implementation does, and what keeps a hook's
// create and delete halves in one file where they can be read against each
// other.
type Kind string

const (
	// Ignition runs at the installer's IgnitionProvider point of extension:
	// after the cluster's infrastructure reports ready and after the bootstrap
	// ignition has had the install info injected, and before any machine is
	// created. Cloud infrastructure therefore exists by this point -- the
	// network and the load balancers -- but no compute does.
	//
	// That is the same point at which the integrated providers stage: the
	// OpenStack provider uploads the bootstrap ignition and returns a shim,
	// IBM Cloud writes it to a COS bucket because it is too large for VSI
	// user data, and Nutanix builds an ISO and uploads it to Prism Central.
	// A provider that needs cloud prerequisites to exist first -- a bucket,
	// a pre-authenticated request -- has them here and would not at an
	// earlier phase.
	//
	// It is the only hook whose output the installer reads back, and the only
	// one handed the cluster's day-0 secret material. It exists for a provider
	// whose cloud cannot carry a full bootstrap ignition in instance metadata
	// -- OCI caps that metadata at 32,000 bytes and CAPOCI base64-encodes into
	// it, so the effective ceiling is nearer 24,000 bytes -- and which has no
	// equivalent of the staging CAPA does inside its own controller.
	Ignition Kind = "ignition"

	// InfraReady runs once the Cluster reports its infrastructure ready and
	// before any machine is created.
	InfraReady Kind = "infra-ready"

	// PostProvision runs once the control-plane machines exist, before the
	// installer waits for the bootstrap to complete.
	PostProvision Kind = "post-provision"

	// PreDestroy runs during `destroy cluster`, before the Cluster is deleted.
	PreDestroy Kind = "pre-destroy"
)

// Request is everything a hook is given.
//
// The fields divide in two. Most describe the cluster in terms the installer
// owns and every platform shares. ClusterJSON and InfraJSON are the exception
// and are the reason a hook can be useful at all: they are the Cluster API
// objects copied verbatim out of the installer's local control plane, so a
// hook can read provider-specific status -- a load balancer's DNS name, a
// network ID -- that the installer has no type for and must never grow one
// for.
type Request struct {
	// Kind is the hook being run.
	Kind Kind

	// Program is the path to the executable, relative to ProgramRoot inside
	// InstallDir.
	Program string

	// ProgramRoot is the directory, relative to InstallDir, that Program is
	// resolved against. Empty means the External manifest directory, which is
	// where a hook the user configured lives.
	//
	// It is a field rather than a constant because a provider payload brings
	// its own hooks, and those are rooted at the directory the payload was
	// extracted into. The containment rule does not change -- a program must
	// still stay inside its root, and the root inside the install directory --
	// only what the root is.
	ProgramRoot string

	// InstallDir is the install directory. It is the hook's working
	// directory and the root its Program is resolved against.
	InstallDir string

	// InfraID is the cluster's infrastructure ID, the name the provider's
	// objects and cloud resources carry.
	InfraID string

	// ClusterName and BaseDomain give the cluster's identity; their join is
	// the cluster domain, which is what DNS records are built from.
	ClusterName string
	BaseDomain  string

	// Publish is the install-config publishing strategy, "External" or
	// "Internal". A hook creating DNS needs it to decide whether a public
	// record is wanted at all.
	Publish string

	// ControlPlaneEndpointHost and ControlPlaneEndpointPort are
	// Cluster.spec.controlPlaneEndpoint. This is the only endpoint in the
	// core Cluster API contract, so it is the one address a hook can rely on
	// without reading provider-specific status.
	ControlPlaneEndpointHost string
	ControlPlaneEndpointPort string

	// ClusterJSON is the core Cluster object as JSON. Empty if unavailable.
	ClusterJSON []byte

	// InfraJSON is the provider's infrastructure object as JSON. Empty when
	// there is not exactly one, since a hook cannot be told which of several
	// was meant.
	InfraJSON []byte

	// BootstrapIgnitionPath is the installer's finished bootstrap ignition.
	// Set for Ignition and nothing else.
	//
	// The hook reads it and must not modify it: it is the installer's own
	// file, and the installer compares what the hook wrote against it.
	BootstrapIgnitionPath string

	// OutputDir is a directory the installer creates for the hook and removes
	// when it returns. Set for Ignition and nothing else.
	//
	// The hook's declared bootstrapOutputPath is relative to it. It is a
	// directory the installer owns rather than a path the hook chooses so
	// that a hook writing a staged ignition -- the whole cluster's day-0
	// secrets in one file -- cannot leave it somewhere that gets archived.
	OutputDir string
}

// clusterDomain is the name the cluster's DNS records hang off.
func (r Request) clusterDomain() string {
	return fmt.Sprintf("%s.%s", r.ClusterName, r.BaseDomain)
}

// Run executes the hook and waits for it to finish.
//
// A non-zero exit fails the install or the destroy. That is the strict
// choice, and it is the right one: a hook exists because something the
// cluster needs is not being created by anything else, so a hook that failed
// means that thing does not exist. The alternative -- warn and continue -- is
// what produces the failure this whole mechanism was built to avoid, an
// install that runs for forty minutes and then dies at a bootstrap stage with
// no line pointing back at the cause.
func Run(ctx context.Context, req Request) error {
	// Everything below needs an absolute install directory, and `--dir
	// install-dir-x` makes a relative one the ordinary case.
	//
	// The exec path is the reason. cmd.Dir makes the child chdir into the
	// install directory before it execs, so a program path that is itself
	// relative gets resolved a second time, against the directory it already
	// contains -- and the install fails with "no such file or directory" for a
	// file the installer has just opened and digested one line earlier.
	//
	// The environment is the same bug seen from the hook's side, and quieter.
	// A relative OPENSHIFT_INSTALL_STATE_DIR resolves against the hook's own
	// working directory, so a hook that does the reasonable thing and creates
	// the directory before writing to it would succeed, write its state
	// somewhere nobody looks, and leave pre-destroy with nothing to read. What
	// the hook created would then never be destroyed.
	installDir, err := filepath.Abs(req.InstallDir)
	if err != nil {
		return fmt.Errorf("failed to resolve the install directory %s: %w", req.InstallDir, err)
	}

	program, err := resolveProgram(installDir, req.ProgramRoot, req.Program)
	if err != nil {
		return err
	}

	stateDir := filepath.Join(installDir, externaltypes.ManifestDir, externaltypes.HookStateDir)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("failed to create the hook state directory %s: %w", stateDir, err)
	}

	// The objects go in a directory that is removed when the hook returns.
	// They are a snapshot for the duration of one call, and leaving them in
	// the install directory would put a provider's whole object -- whose
	// contents the installer does not understand and cannot vouch for -- into
	// something that gets archived and shared.
	objDir, err := os.MkdirTemp("", "openshift-install-hook-")
	if err != nil {
		return fmt.Errorf("failed to create a temporary directory for the hook input: %w", err)
	}
	defer os.RemoveAll(objDir)

	env := os.Environ()
	env = append(env,
		"OPENSHIFT_INSTALL_HOOK="+string(req.Kind),
		"OPENSHIFT_INSTALL_INFRA_ID="+req.InfraID,
		"OPENSHIFT_INSTALL_CLUSTER_NAME="+req.ClusterName,
		"OPENSHIFT_INSTALL_BASE_DOMAIN="+req.BaseDomain,
		"OPENSHIFT_INSTALL_CLUSTER_DOMAIN="+req.clusterDomain(),
		"OPENSHIFT_INSTALL_PUBLISH="+req.Publish,
		"OPENSHIFT_INSTALL_DIR="+installDir,
		"OPENSHIFT_INSTALL_MANIFEST_DIR="+filepath.Join(installDir, externaltypes.ManifestDir),
		"OPENSHIFT_INSTALL_STATE_DIR="+stateDir,
		"OPENSHIFT_INSTALL_CONTROL_PLANE_ENDPOINT_HOST="+req.ControlPlaneEndpointHost,
		"OPENSHIFT_INSTALL_CONTROL_PLANE_ENDPOINT_PORT="+req.ControlPlaneEndpointPort,
		// The path the installer writes the cluster's admin kubeconfig to
		// (pkg/asset/kubeconfig/admin.go:13). It is set for every hook and is
		// usable by none of them until the API is serving, which is a
		// property of when the hook runs rather than of the variable: at
		// infra-ready nothing has booted, at post-provision the control plane
		// is up. A hook that needs it should say so when it is not usable
		// rather than assume it is.
		"OPENSHIFT_INSTALL_KUBECONFIG="+filepath.Join(installDir, "auth", "kubeconfig"),
		// A path the hook MAY write one line to, which this function quotes
		// in the error it returns. It lives in the temporary directory and
		// goes away with it, because its content is for the error, not for
		// the install directory.
		//
		// This exists because the alternative does not work. A hook's failure
		// is recorded in a file the installer names but does not read, so the
		// installer's own log -- the thing attached to a support case -- says
		// which phase failed and not why, and answering that costs a round
		// trip to whoever still has the install directory. Reading the log
		// automatically is not available either: the Ignition phase's natural
		// output is a pre-signed URL over the whole bootstrap ignition, so
		// "quote the hook's last line" would publish a credential for one
		// phase out of four, under a rule its author cannot see.
		//
		// One line the author chose has neither problem, and it is uniform
		// across phases.
		"OPENSHIFT_INSTALL_HOOK_FAILURE_FILE="+filepath.Join(objDir, "failure-reason"),
	)

	// Two variables nothing but Ignition sees. They are set from the
	// Request rather than derived here because only the caller knows whether
	// this phase is the one that stages ignition, and a hook that found them
	// set at infra-ready would reasonably conclude it was expected to act.
	if req.BootstrapIgnitionPath != "" {
		env = append(env, "OPENSHIFT_INSTALL_BOOTSTRAP_IGNITION="+req.BootstrapIgnitionPath)
	}
	if req.OutputDir != "" {
		env = append(env, "OPENSHIFT_INSTALL_HOOK_OUTPUT_DIR="+req.OutputDir)
	}

	for _, obj := range []struct {
		envVar string
		file   string
		data   []byte
	}{
		{"OPENSHIFT_INSTALL_CLUSTER_JSON", "cluster.json", req.ClusterJSON},
		{"OPENSHIFT_INSTALL_INFRA_JSON", "infrastructure.json", req.InfraJSON},
	} {
		if len(obj.data) == 0 {
			continue
		}
		path := filepath.Join(objDir, obj.file)
		if err := os.WriteFile(path, obj.data, 0o600); err != nil {
			return fmt.Errorf("failed to write the hook input %s: %w", path, err)
		}
		env = append(env, obj.envVar+"="+path)
	}

	// Path and digest, never contents: the same rule the provider artifact
	// resolver follows. The digest is what lets a failed install and a failed
	// destroy be compared -- two runs against different versions of a hook is
	// otherwise invisible.
	digest, err := sha256File(program)
	if err != nil {
		return err
	}
	// A hook has no arguments. The payload declares a program and nothing
	// else, so the command is fully described by the path and the digest --
	// which also closes, by construction, the one way an argument list could
	// put a credential into the installer log.
	logFile, err := openLog(installDir, req.Kind)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logPath := logFile.Name()

	logrus.Infof("Running the %s hook %s (sha256:%s); its output is written to %s",
		req.Kind, program, digest, logPath)

	cmd := exec.CommandContext(ctx, program)
	cmd.Dir = installDir
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// The banners mark, in the installer's own log, the window in which code
	// the installer did not write and cannot vouch for is running. Everything
	// between them is the provider's: the installer chose the moment and the
	// inputs, the provider chose what happens.
	//
	// This is not decoration. `platform: external` is a shared-responsibility
	// contract, and the usual way that goes wrong is a support conversation
	// about a failure inside a partner's hook that reads, from the log alone,
	// like a failure in OpenShift. A reader scanning for the cause of a broken
	// install should be able to see which side of the line they are on without
	// knowing what a hook is.
	//
	// Both banners carry the phase, so two hooks cannot be confused for one,
	// and the closing one is emitted whether the hook succeeded or failed --
	// an unterminated window would be read as "the installer never came back",
	// which is the opposite of what a hook failure means.
	logrus.Infof("=== STARTING PROVIDER HOOK (%s) ===", req.Kind)
	runErr := cmd.Run()
	logrus.Infof("=== FINISHING PROVIDER HOOK (%s) ===", req.Kind)

	if runErr != nil {
		// The hook's *output* is still deliberately not interpolated into
		// this error. It used to be, and for the three original phases it was
		// the right trade: a hook is the only thing that knows why it failed.
		// Ignition changed the arithmetic -- its natural debug output is
		// a pre-signed URL over the whole bootstrap ignition -- and a rule
		// that quoted some phases and not others would be a rule the hook
		// author cannot see from their side.
		//
		// What is interpolated is the one line the hook chose to write to
		// OPENSHIFT_INSTALL_HOOK_FAILURE_FILE, which is not the same thing:
		// the author decides the content, so the rule is uniform across
		// phases and visible from the side that has to satisfy it. The log
		// path is still given either way, because the summary is a pointer to
		// the cause and not a replacement for it.
		reason := hookFailureReason(filepath.Join(objDir, "failure-reason"))
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			if reason != "" {
				return fmt.Errorf("the %s hook %s exited %d: %s (its full output is in %s)",
					req.Kind, req.Program, exitErr.ExitCode(), reason, logPath)
			}
			return fmt.Errorf("the %s hook %s exited %d; its output is in %s",
				req.Kind, req.Program, exitErr.ExitCode(), logPath)
		}
		return fmt.Errorf("failed to run the %s hook %s: %w (its output, if any, is in %s)",
			req.Kind, req.Program, runErr, logPath)
	}
	logrus.Infof("The %s hook %s succeeded", req.Kind, req.Program)
	return nil
}

// maxHookFailureReason bounds what a hook can put into the installer's error.
//
// A limit rather than trust: the file is written by a program the installer
// did not author, and an error message is read by a person. A hook that
// redirects its whole stdout here should get its first line quoted and
// truncated, not turn one error into a page of scrollback.
const maxHookFailureReason = 300

// hookFailureReason returns the single line a hook chose to explain itself
// with, or "" if it wrote nothing, could not be read, or had nothing to say.
//
// Every failure mode here returns "", and that is deliberate: this is a
// courtesy on top of an error that already exists and is already correct.
// Failing to read it must never become a second error competing with the
// first, which is the one the user actually needs.
func hookFailureReason(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	line = strings.TrimSpace(line)
	// Truncated by runes, not bytes, so a multi-byte character cannot be cut
	// in half and turn the message into mojibake at exactly the moment
	// somebody is trying to read it.
	if r := []rune(line); len(r) > maxHookFailureReason {
		line = string(r[:maxHookFailureReason]) + "..."
	}
	return line
}

// openLog creates this phase's log file, truncating any earlier one.
//
// Truncating rather than appending is deliberate: a phase runs once per
// install, and a file holding two runs is one a reader has to date-stamp
// before they can tell which failure they are looking at. The previous run's
// output is not something the installer should be keeping anyway.
//
// 0600 on the file and 0700 on the directory, because the contents are
// whatever the hook chose to print and the installer has no way to know what
// that is.
func openLog(installDir string, kind Kind) (*os.File, error) {
	dir := filepath.Join(installDir, externaltypes.HookLogDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create the hook log directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, string(kind)+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to create the hook log %s: %w", path, err)
	}
	return f, nil
}

// resolveProgram turns the configured relative path into an absolute one,
// refusing anything that leaves its root or is not executable.
//
// installDir must already be absolute. The result is handed to exec.Cmd whose
// Dir is that same directory, and a relative result would then be resolved
// against it a second time; Run calls filepath.Abs before getting here.
//
// Both checks are here rather than at validation time because both need the
// filesystem, and because this is the last moment before the program runs --
// a hook that was executable when the install-config was validated and is not
// now should fail with the same message.
func resolveProgram(installDir, root, program string) (string, error) {
	if program == "" {
		return "", fmt.Errorf("no hook program configured")
	}
	if root == "" {
		root = externaltypes.ManifestDir
	}
	// The root is localized for the same reason the program is: it reaches
	// here from a payload's own layout by way of metadata.json, so it is not
	// the installer's own string by the time destroy reads it back.
	localRoot, err := filepath.Localize(root)
	if err != nil {
		return "", fmt.Errorf("the hook directory %q must be a relative path inside the install directory: %w",
			root, err)
	}

	local, err := filepath.Localize(program)
	if err != nil {
		return "", fmt.Errorf("hook %q must be a relative path inside the %q directory: %w",
			program, localRoot, err)
	}
	path := filepath.Join(installDir, localRoot, local)

	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return "", fmt.Errorf("hook %s does not exist: it is named in "+
			"platform.external.clusterAPI.hooks or in the provider payload's layout and is expected at that "+
			"path, relative to the %q directory of the install directory", path, localRoot)
	case err != nil:
		return "", fmt.Errorf("failed to read the hook %s: %w", path, err)
	case info.IsDir():
		return "", fmt.Errorf("hook %s is a directory", path)
	case info.Mode().Perm()&0o111 == 0:
		return "", fmt.Errorf("hook %s is not executable: run chmod +x on it. "+
			"The installer executes it directly rather than through a shell, so the execute bit and, for a "+
			"script, a #! line are both required", path)
	}
	return path, nil
}

// sha256File digests a file without retaining its contents.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("failed to open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("failed to read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
