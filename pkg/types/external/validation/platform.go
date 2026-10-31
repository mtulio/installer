// Package validation validates the External platform section of the
// install-config.
package validation

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/external"
)

// ValidatePlatform checks that the specified platform is valid.
func ValidatePlatform(p *external.Platform, fldPath *field.Path, c *types.InstallConfig) field.ErrorList {
	allErrs := field.ErrorList{}
	if p == nil {
		return allErrs
	}
	allErrs = append(allErrs, validateClusterAPI(p.ClusterAPI, fldPath.Child("clusterAPI"), c)...)
	return allErrs
}

// validateClusterAPI checks the user-supplied Cluster API infrastructure
// provider.
//
// There are two modes and they are a union, not two independent field sets.
// In the default "local" mode the binary and the components are paths on this
// machine, and both are required together: a provider with no binary cannot be
// started, and one with no components has no CRDs, so the manifests the user
// supplies would be rejected by the local control plane. In "image" mode every
// artifact comes from the payload and no local path is consulted, so supplying
// one is an error rather than something to ignore -- a path that looks
// configured and is silently unused is the failure worth catching early.
//
// Only the shape of the configuration is checked here. Whether the paths
// resolve to a usable executable, whether the image exists, and whether the
// payload's declared layout is present are decided later, by the resolver in
// pkg/infrastructure/external/payload: those checks need the filesystem or
// the network, and duplicating them here would mean two implementations
// disagreeing about what is valid.
func validateClusterAPI(p *external.ClusterAPIProvider, fldPath *field.Path, c *types.InstallConfig) field.ErrorList {
	allErrs := field.ErrorList{}
	if p == nil {
		return allErrs
	}
	if p.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), "provider name is required"))
	}

	// One list of controller arguments, written in one place. Merging two
	// spellings would be the kind of fact-written-twice that drifts apart
	// without anyone noticing which copy is in effect.
	if len(p.Args) > 0 && p.Controller != nil && len(p.Controller.Args) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("controller", "args"), p.Controller.Args,
			"args is set both here and at clusterAPI.args; set only one"))
	}

	if p.UsesImagePayload() {
		allErrs = append(allErrs, validateImageMode(p, fldPath, c)...)
	} else {
		allErrs = append(allErrs, validateLocalMode(p, fldPath)...)
	}

	allErrs = append(allErrs, validateVariables(p, fldPath.Child("variables"))...)
	// The External manifest directory in both modes: what is validated here is
	// a hook the install-config names, and that is where such a hook lives
	// whether or not a payload is in play. A payload's own hooks are declared
	// in its descriptor, checked against the extracted tree by
	// Descriptor.Validate, and resolved against the extraction directory --
	// they never reach this function.
	allErrs = append(allErrs, validateHooks(p.Hooks, fldPath.Child("hooks"), external.ManifestDir)...)
	return allErrs
}

// digestReference matches a reference pinned by digest rather than by tag.
var digestReference = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// validateImageMode checks the payload-image configuration.
func validateImageMode(p *external.ClusterAPIProvider, fldPath *field.Path, c *types.InstallConfig) field.ErrorList {
	allErrs := field.ErrorList{}
	ctrl := fldPath.Child("controller")

	if p.Controller.Image == "" {
		allErrs = append(allErrs, field.Required(ctrl.Child("image"),
			`image is required when sourceType is "image"`))
	} else {
		allErrs = append(allErrs, validateImageReference(p.Controller.Image, ctrl.Child("image"), c)...)
	}

	// The local-path fields are not merely ignored in this mode; accepting
	// them would make it ambiguous which controller runs, and the payload is
	// meant to be one unit of trust.
	for _, f := range []struct {
		path  *field.Path
		value string
	}{
		{fldPath.Child("binaryPath"), p.BinaryPath},
		{fldPath.Child("componentsPath"), p.ComponentsPath},
		{ctrl.Child("path"), p.Controller.Path},
	} {
		if f.value != "" {
			allErrs = append(allErrs, field.Invalid(f.path, f.value,
				`must not be set when controller.sourceType is "image": the payload supplies every artifact`))
		}
	}
	return allErrs
}

// validateImageReference requires a fully qualified reference, and a digest
// when the install is mirrored.
//
// imageDigestSources is digest-oriented by name and by what replaced its
// tag-handling predecessor, so rather than reintroduce tag mirroring the rule
// is narrow: a tag is fine for a connected install, and a mirrored one must
// pin a digest. The install log records the resolved digest either way, so a
// tagged install is still traceable after the fact.
func validateImageReference(ref string, fldPath *field.Path, c *types.InstallConfig) field.ErrorList {
	allErrs := field.ErrorList{}

	domain, _, found := strings.Cut(ref, "/")
	if !found || !(strings.ContainsAny(domain, ".:") || domain == "localhost") {
		allErrs = append(allErrs, field.Invalid(fldPath, ref,
			"must be a fully qualified image reference including the registry host, "+
				"for example quay.io/example/provider-payload:v1"))
		return allErrs
	}

	if c != nil && len(c.ImageDigestSources) > 0 && !digestReference.MatchString(ref) {
		allErrs = append(allErrs, field.Invalid(fldPath, ref,
			"must be pinned by digest when imageDigestSources is set, because mirrors are resolved by digest; "+
				"use registry/repository@sha256:<digest>"))
	}
	return allErrs
}

// validateLocalMode checks the original, path-based configuration. Its rules
// are unchanged: this is the behaviour that existed before payload images, and
// it has to keep working exactly as it did.
func validateLocalMode(p *external.ClusterAPIProvider, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	binary := p.BinaryPath
	if p.Controller != nil && p.Controller.Path != "" {
		if binary != "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("controller", "path"), p.Controller.Path,
				"the controller binary is set both here and at clusterAPI.binaryPath; set only one"))
		}
		binary = p.Controller.Path
	}
	if binary == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("binaryPath"),
			"path to the provider controller binary is required"))
	}
	if p.ComponentsPath == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("componentsPath"),
			"path to the provider components is required"))
	}
	if p.Controller != nil && p.Controller.Image != "" {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("controller", "image"), p.Controller.Image,
			`must not be set unless controller.sourceType is "image"`))
	}
	// These three describe a payload's internal layout and have nothing to
	// resolve against when there is no payload.
	for _, f := range []struct {
		name  string
		value string
	}{
		{"capiInstallerPath", p.CapiInstallerPath},
		{"customResource", p.CustomResource},
		{"manifestPath", p.ManifestPath},
	} {
		if f.value != "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child(f.name), f.value,
				`is only meaningful when controller.sourceType is "image"`))
		}
	}
	return allErrs
}

// variableName is the set of names a template token can have, so that a value
// supplied here can actually be referenced by one.
var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateVariables checks the substitution values.
//
// Variables only have a consumer in image mode, where the payload ships the
// templates. Rejecting them otherwise rather than ignoring them follows the
// same reasoning as the hook rules below: a setting the user believes is in
// effect, and that nothing reads, is worth an error.
func validateVariables(p *external.ClusterAPIProvider, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(p.Variables) == 0 {
		return allErrs
	}
	if !p.UsesImagePayload() {
		allErrs = append(allErrs, field.Invalid(fldPath, p.Variables,
			`is only meaningful when controller.sourceType is "image": nothing renders templates in local mode`))
		return allErrs
	}
	for name := range p.Variables {
		if !variableName.MatchString(name) {
			allErrs = append(allErrs, field.Invalid(fldPath.Key(name), name,
				"must be a valid substitution variable name, matching "+variableName.String()))
			continue
		}
		if strings.HasPrefix(name, payloadInstallerVariablePrefix) {
			allErrs = append(allErrs, field.Invalid(fldPath.Key(name), name,
				fmt.Sprintf("names beginning with %q are supplied by the installer and cannot be set here",
					payloadInstallerVariablePrefix)))
		}
	}
	return allErrs
}

// payloadInstallerVariablePrefix duplicates payload.InstallerVariablePrefix.
//
// Duplicated rather than imported to keep the layering one-way: validation of
// an install-config is the bottom of the stack and is imported by anything
// that merely wants to check one, while the payload package reads images and
// writes to the install directory. Depending upwards for a ten-character
// string would be the more expensive of the two mistakes.
//
// TestVariablePrefixMatchesPayload asserts the two agree. It lives in this
// package's tests, where importing upwards costs nothing.
const payloadInstallerVariablePrefix = "INSTALLER_"

// validateHooks checks the shape of each configured hook.
//
// Only containment is decided here, not existence or executability: those
// need the install directory, which this package does not have, and they are
// checked where the hook is about to run. Containment can be decided from the
// string alone, and has to be, because it is the property that makes the
// install directory a self-contained unit.
//
// Args are deliberately not validated at all. They are the partner's
// contract with their own program, and the installer does not know what a
// valid one looks like -- inventing a rule here would reject a working
// configuration for a program this code has never seen.
func validateHooks(h *external.Hooks, fldPath *field.Path, root string) field.ErrorList {
	allErrs := field.ErrorList{}
	if h == nil {
		return allErrs
	}
	for _, hook := range []struct {
		name string
		hook *external.Hook
	}{
		{"infraReady", h.InfraReady},
		{"postProvision", h.PostProvision},
		{"preDestroy", h.PreDestroy},
	} {
		allErrs = append(allErrs, validateHook(hook.hook, fldPath.Child(hook.name), root)...)
	}
	return allErrs
}

// validateHook requires a program, given as a relative path that stays inside
// the External manifest directory.
//
// filepath.Localize is the containment check rather than a hand-written scan
// for "..": it rejects absolute paths, parent traversal and platform-specific
// escapes in one place, and it is the same function the standard library uses
// to decide whether a path from an untrusted source can be joined to a root.
func validateHook(h *external.Hook, fldPath *field.Path, root string) field.ErrorList {
	allErrs := field.ErrorList{}
	if h == nil {
		return allErrs
	}
	// An empty program with arguments set is the mistake worth catching: it
	// is a hook the user believes they configured and that would silently
	// never run.
	if h.Program == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("program"),
			"path to the hook program is required when the hook is configured"))
		return allErrs
	}
	if _, err := filepath.Localize(h.Program); err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("program"), h.Program,
			fmt.Sprintf("must be a relative path inside the %q directory, so that the hook travels with "+
				"the unit it belongs to", root)))
	}
	return allErrs
}
