package payload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	// maxPayloadBytes bounds what a payload may occupy on disk.
	//
	// 8 GiB is far above any plausible payload -- a provider controller and
	// some YAML -- and far below anything that would fill a normal install
	// host.
	//
	// The bound is checked after extraction rather than during it, because
	// `oc image extract` writes the tree itself and offers no size limit. It
	// is therefore a sanity check on what arrived, not a guard on what is
	// written. The same is true of every other `oc image extract` call in the
	// installer, which bound nothing at all.
	maxPayloadBytes = 8 << 30

	// ocTries and ocRetryDelay mirror pkg/asset/rhcos, which pulls from a
	// registry under the same conditions and settled on these.
	ocTries      = 5
	ocRetryDelay = 5 * time.Second
)

// Fetch pulls a provider payload image, extracts it into dest, and returns the
// digest the reference resolved to.
//
// The digest is logged and returned whatever the reference looked like, so a
// tagged install remains traceable after the fact even though it is not
// reproducible from the install-config alone.
//
// Authentication reuses the install-config pullSecret and redirection reuses
// imageDigestSources; neither needed a new mechanism, because both problems
// already have one that works before a cluster exists.
//
// # Why this shells out to `oc` rather than speaking to the registry
//
// It was an in-process go-containerregistry client first. That worked, and it
// cost 22,861 vendored lines across seven modules -- docker/cli, containerd's
// estargz, vbatts/tar-split and four others -- to support 1,547 lines of
// payload handling. `mutate.Extract` alone accounted for about 8,000 of them.
//
// The installer already pulls files out of images, and has for years, without
// any of that: pkg/asset/rhcos/releaseextract.go runs `oc image extract` as a
// subprocess. Not only on the agent path -- pkg/infrastructure/baremetal/
// bootstrap.go:175 does it during `create cluster`, which is this code's own
// phase. Doing it a second, different way would have meant two registry
// clients in one binary disagreeing about mirrors, proxies and auth.
//
// The cost of the swap is a runtime dependency on `oc`, which is checked for
// up front and only when a payload image is actually configured, and the loss
// of the executable bit -- see Descriptor.ApplyExecutableModes.
func Fetch(ctx context.Context, ref string, dest string, pullSecret string, mirrors map[string][]string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", errors.New("no provider payload image was configured")
	}
	if err := requireOC(); err != nil {
		return "", err
	}

	// Shared by both calls so that the digest reported is the digest of the
	// image that was extracted, resolved through the same mirrors and the
	// same architecture filter.
	//
	// linux/GOARCH, not the cluster's architecture: the controller in this
	// payload is run as a local process on the host doing the install
	// (pkg/clusterapi/internal/process), not scheduled into the cluster. A
	// payload built only for the cluster's architecture would pass every
	// check here and then fail to exec.
	common := []string{"--filter-by-os=linux/" + runtime.GOARCH}
	if insecurePull(ref) {
		// Narrow on purpose. `oc image extract` elsewhere in the installer
		// passes --insecure=true unconditionally; that is not a precedent
		// worth inheriting for an artifact whose whole point is to be a
		// provider's signed, digest-pinned unit of trust. A loopback registry
		// is the development case the install-config validator already
		// singles out by name (pkg/types/external/validation/platform.go:125),
		// and nothing reaches it over a network that could be intercepted.
		logrus.Debugf("Allowing a plain-HTTP pull of %s because it is on a loopback registry", ref)
		common = append(common, "--insecure=true")
	}

	infoArg, cleanupInfo, err := mirrorArg(mirrors, icspFormat)
	if err != nil {
		return "", err
	}
	defer cleanupInfo()
	extractArg, cleanupExtract, err := mirrorArg(mirrors, idmsFormat)
	if err != nil {
		return "", err
	}
	defer cleanupExtract()

	digest, err := resolveDigest(ctx, ref, pullSecret, withArg(common, infoArg))
	if err != nil {
		return "", err
	}
	logrus.Infof("Provider payload %s resolved to digest %s", ref, digest)

	if err := extract(ctx, ref, dest, pullSecret, withArg(common, extractArg)); err != nil {
		return "", err
	}
	if err := checkSize(dest); err != nil {
		return "", err
	}
	logrus.Infof("Extracted the provider payload into %s", dest)
	return digest, nil
}

// withArg appends an optional flag without aliasing the shared slice.
func withArg(common []string, arg string) []string {
	out := append([]string(nil), common...)
	if arg != "" {
		out = append(out, arg)
	}
	return out
}

// requireOC fails early, and only on this path.
//
// The check is here rather than in install-config validation so that it costs
// nothing to anyone not using a payload image: no other platform, and not the
// local-path mode of this one, acquires a dependency on `oc` by this change.
// Failing at `create manifests` rather than part-way through `create cluster`
// is the point -- by then the user has waited, and on some providers paid.
func requireOC() error {
	if _, err := exec.LookPath("oc"); err != nil {
		return fmt.Errorf("the \"oc\" command is required to install from a provider payload image and was not "+
			"found on PATH: the installer extracts the payload with `oc image extract`. Install the OpenShift "+
			"client, or use platform.external.clusterAPI.binaryPath and componentsPath to supply the provider "+
			"from local paths instead: %w", err)
	}
	return nil
}

// imageInfo is the part of `oc image info -o json` this reads.
type imageInfo struct {
	Digest string `json:"digest"`
	// ListDigest is set when the reference resolved to a manifest list. It is
	// preferred over Digest because it is the digest of the thing the user
	// named: pinning the per-architecture Digest instead would produce a
	// reference that only installs on one architecture, which is not what a
	// recorded provenance value should mean.
	ListDigest string `json:"listDigest"`
}

func resolveDigest(ctx context.Context, ref, pullSecret string, common []string) (string, error) {
	cmd := append([]string{"oc", "image", "info", "--output=json"}, common...)
	cmd = append(cmd, ref)

	out, err := runOC(ctx, pullSecret, cmd, 1)
	if err != nil {
		return "", fmt.Errorf("failed to read the provider payload image %s: %w", ref, err)
	}
	info := imageInfo{}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return "", fmt.Errorf("could not parse `oc image info` output for %s: %w", ref, err)
	}
	if info.ListDigest != "" {
		return info.ListDigest, nil
	}
	if info.Digest == "" {
		return "", fmt.Errorf("`oc image info` reported no digest for %s", ref)
	}
	return info.Digest, nil
}

// extract writes the whole image into dest.
//
// --path=/:dest takes the entire filesystem rather than named members: the
// descriptor decides what the payload contains, and extracting only declared
// paths would hide an undeclared one from Descriptor.Validate, which is the
// check that makes the declaration complete.
//
// --only-files drops everything that is not a regular file or a directory.
// That is the same rule the previous in-process extractor applied by hand on
// the tar type flag, for the same reason: a payload is data plus a controller
// binary, and a device node, socket or hard link in one is a sign of
// something other than a payload. It also means no symlink is written, so no
// extracted path can later resolve outside dest.
func extract(ctx context.Context, ref, dest, pullSecret string, common []string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	cmd := append([]string{"oc", "image", "extract", "--path=/:" + abs, "--only-files", "--confirm"}, common...)
	cmd = append(cmd, ref)

	if _, err := runOC(ctx, pullSecret, cmd, ocTries); err != nil {
		return fmt.Errorf("failed to extract the provider payload %s: %w", ref, err)
	}
	return nil
}

// runOC executes an oc command, retrying transient failures.
//
// This duplicates about fifteen lines of agent.ExecuteOC, and the duplication
// is the lesser of two costs. Importing pkg/asset/agent from here creates an
// import cycle -- it reaches pkg/types/validation, which reaches
// pkg/types/external/validation, whose tests import this package -- and the
// clean fix, lifting the helper into a package below both, would edit the
// agent and rhcos install paths. Nothing about a payload image should require
// touching those.
//
// Two differences from the original are deliberate rather than accidental:
// the pull secret file is created 0600 explicitly instead of relying on
// os.CreateTemp's default, and the command runs under the context, so a
// Ctrl-C during a slow pull is honoured mid-layer rather than only between
// attempts.
func runOC(ctx context.Context, pullSecret string, cmd []string, tries int) (string, error) {
	// Paths and flags only. The pull secret never appears in a command line:
	// it reaches oc through --registry-config, as a file.
	logrus.Debugf("Running %s", strings.Join(cmd, " "))

	var lastErr error
	for attempt := 1; attempt <= tries; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		out, err := execOC(ctx, pullSecret, cmd)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if attempt < tries {
			logrus.Debugf("Attempt %d of %d failed, retrying in %s: %v", attempt, tries, ocRetryDelay, err)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(ocRetryDelay):
			}
		}
	}
	return "", lastErr
}

// emptyRegistryConfig is a valid docker config that authenticates to nothing.
//
// It is what an empty pullSecret becomes, and the alternative -- leaving
// --registry-config off the command line -- is the thing to avoid. Without
// the flag, oc falls back to ambient credentials: $XDG_RUNTIME_DIR/containers/
// auth.json, $DOCKER_CONFIG, ~/.docker/config.json, and from there to any
// credential helper those name. The in-process client this replaced went out
// of its way not to do that, so that the install-config is the only credential
// source and the same install-config pulls the same image on another machine.
// Passing an empty document keeps that property through the subprocess.
const emptyRegistryConfig = `{"auths":{}}`

// execOC runs one oc invocation with the pull secret supplied as a registry
// config file.
//
// The file is written 0600 and removed on every path. Its contents are the
// pull secret verbatim, which is why nothing here logs the file, its size or
// anything derived from it -- only its path, which is not a secret.
func execOC(ctx context.Context, pullSecret string, cmd []string) (string, error) {
	config := pullSecret
	if strings.TrimSpace(config) == "" {
		// A public payload on an anonymous-readable registry is a legitimate
		// configuration, and the loopback registry used for development is
		// one. It is not a reason to start reading the user's docker config.
		config = emptyRegistryConfig
	}

	f, err := os.CreateTemp("", "payload-registry-config")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return "", err
	}
	if _, err := f.WriteString(config); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	args := append(append([]string(nil), cmd[1:]...), "--registry-config="+f.Name())
	var stdout, stderr strings.Builder
	//nolint:gosec // cmd[0] is the literal "oc"; every argument is built in this file from paths and flags.
	c := exec.CommandContext(ctx, cmd[0], args...)
	c.Stdout = &stdout
	c.Stderr = &stderr

	if err := c.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// stderr, not stdout: oc reports failures there, and on a
			// successful-but-unparsed run stdout could be large.
			return "", fmt.Errorf("%s exited %d: %s", strings.Join(cmd, " "), exitErr.ExitCode(),
				strings.TrimSpace(stderr.String()))
		}
		return "", fmt.Errorf("%s failed: %w", strings.Join(cmd, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// checkSize rejects an implausibly large payload after the fact.
func checkSize(dest string) error {
	var total int64
	err := filepath.WalkDir(dest, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > maxPayloadBytes {
			return fmt.Errorf("provider payload expands to more than %d bytes", int64(maxPayloadBytes))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to inspect the extracted provider payload at %s: %w", dest, err)
	}
	return nil
}

// insecurePull reports whether the reference names a loopback registry.
//
// Matching on the registry host alone is deliberate: "localhost:5000/x" is a
// loopback registry and "example.com/localhost/x" is not, and the difference
// is the first path segment.
func insecurePull(ref string) bool {
	domain, _, found := strings.Cut(ref, "/")
	if !found {
		return false
	}
	// An IPv6 host is bracketed, and its own colons must not be mistaken for
	// the port separator: "[::1]:5000" splits after the bracket, not at the
	// first colon.
	host := domain
	if strings.HasPrefix(domain, "[") {
		if end := strings.Index(domain, "]"); end >= 0 {
			host = domain[1:end]
		}
	} else if i := strings.Index(domain, ":"); i >= 0 {
		host = domain[:i]
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// mirrorFormat is one subcommand's way of being told about mirrors.
//
// There are two because oc 5.0 is not consistent between its own
// subcommands: `oc image extract` accepts --idms-file and rejects
// --icsp-file, and `oc image info` accepts --icsp-file and rejects
// --idms-file. The content differs with the flag -- ImageDigestMirrorSet and
// ImageContentSourcePolicy are different kinds in different API groups -- so
// one file cannot serve both. This is oc's shape, not a choice made here, and
// it is written down because the obvious simplification of passing the same
// flag to both fails only when mirrors are configured, which is the one case
// that is never exercised locally.
type mirrorFormat struct {
	flag       string
	apiVersion string
	kind       string
	field      string
}

var (
	// idmsFormat is for `oc image extract`.
	idmsFormat = mirrorFormat{"--idms-file", "config.openshift.io/v1", "ImageDigestMirrorSet", "imageDigestMirrors"}
	// icspFormat is for `oc image info`, which still speaks the older kind.
	icspFormat = mirrorFormat{
		"--icsp-file", "operator.openshift.io/v1alpha1", "ImageContentSourcePolicy", "repositoryDigestMirrors",
	}
)

// mirrorArg writes imageDigestSources out in the format the given subcommand
// accepts and returns the flag pointing at it.
//
// Resolution is left to oc rather than done here by rewriting the reference
// and trying each candidate in turn, which is what the in-process client did.
// oc applies the same mapping the cluster will, including its ordering and
// fallback rules, so a mirrored payload install resolves the way a mirrored
// anything else does. Getting that subtly different in a second
// implementation is the failure this avoids.
func mirrorArg(mirrors map[string][]string, format mirrorFormat) (string, func(), error) {
	noop := func() {}
	if len(mirrors) == 0 {
		return "", noop, nil
	}

	sources := make([]string, 0, len(mirrors))
	for source := range mirrors {
		sources = append(sources, source)
	}
	sort.Strings(sources)

	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: %s\nmetadata:\n  name: provider-payload\nspec:\n  %s:\n",
		format.apiVersion, format.kind, format.field)
	for _, source := range sources {
		fmt.Fprintf(&b, "  - source: %q\n    mirrors:\n", source)
		for _, mirror := range mirrors[source] {
			fmt.Fprintf(&b, "    - %q\n", mirror)
		}
	}

	f, err := os.CreateTemp("", "payload-mirrors")
	if err != nil {
		return "", noop, err
	}
	// f.Name() is the path os.CreateTemp just chose, not anything derived
	// from the mirror configuration; the only thing taken from the user here
	// is the file's contents.
	cleanup := func() { os.Remove(f.Name()) } //nolint:gosec // temp file created immediately above.
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		cleanup()
		return "", noop, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", noop, err
	}
	return format.flag + "=" + f.Name(), cleanup, nil
}
