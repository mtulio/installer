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
// There is one supply mode: a payload image. The pilot also accepted a
// controller binary and a components file as paths on this machine, and that
// is gone -- not because it did not work, but because it made the provider
// several independently-chosen artifacts instead of one unit, with nothing
// recording which combination was installed. A partner with a binary on disk
// builds a payload image around it.
//
// What remains here is the shape of the configuration. Whether the image
// exists, whether the payload's declared layout is present, and whether the
// controller's command line is well formed are all decided later, by the
// resolver in pkg/infrastructure/external/payload and by Descriptor.Load:
// those need the network or the extracted tree, and duplicating them here
// would mean two implementations disagreeing about what is valid.
//
// That split has one consequence worth naming. The payload's metadata.name
// must equal platform.external.platformName, and the payload's ignition
// hook requires bootstrapIgnitionStaging: Allowed -- neither can be checked
// from the install-config alone, so both are enforced at resolve time, after
// the image is pulled and before any cloud call. They are refusals, but they
// are later refusals than the ones in this file.
func validateClusterAPI(p *external.ClusterAPIProvider, fldPath *field.Path, c *types.InstallConfig) field.ErrorList {
	allErrs := field.ErrorList{}
	if p == nil {
		return allErrs
	}

	if p.PayloadImage == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("payloadImage"),
			"a provider payload image is required to provision with Cluster API"))
	} else {
		allErrs = append(allErrs, validateImageReference(p.PayloadImage, fldPath.Child("payloadImage"), c)...)
	}

	allErrs = append(allErrs, validateControllerExtraArgs(p.ControllerExtraArgs,
		fldPath.Child("controllerExtraArgs"))...)
	allErrs = append(allErrs, validateBootstrapIgnitionStaging(p.BootstrapIgnitionStaging,
		fldPath.Child("bootstrapIgnitionStaging"))...)
	allErrs = append(allErrs, validatePayloadPaths(p, fldPath)...)
	allErrs = append(allErrs, validateVariables(p, fldPath.Child("variables"))...)
	return allErrs
}

// digestReference matches a reference pinned by digest rather than by tag.
var digestReference = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

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

// renderToken matches the installer's template syntax in a value that is not
// rendered.
var renderToken = regexp.MustCompile(`{{[^}]*}}`)

// validateControllerExtraArgs checks the arguments appended to the payload's
// controller command line.
//
// These are appended verbatim, after the payload's own spec.capi.controllerArgs
// and before the --kubeconfig the installer adds, so the last occurrence of a
// repeated flag is this one. That is what makes "-v=5" work for a support case
// without repackaging the image, and it is the whole intended use.
//
// What is refused is a render token. The payload's list is passed through
// text/template so that a provider can declare a flag whose value only the
// installer knows -- the webhook port, the certificate directory, a free health
// port -- and that happens to render this list too. Those inputs are an
// internal detail of how the local control plane is started, not a user
// interface, and a cluster creator who writes one is either duplicating a value
// the payload already declared, which breaks the exactly-once rule the payload
// was validated against, or naming an input that does not exist, which renders
// to the empty string and produces a flag with no value rather than an error.
// Refusing the whole syntax is both checks at once, and it does not have to be
// revisited every time a render input is added or removed.
func validateControllerExtraArgs(args []string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for i, a := range args {
		if renderToken.MatchString(a) {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(i), a,
				"must not contain a {{...}} template token: the installer's render inputs are supplied to "+
					"the payload's spec.capi.controllerArgs and are not part of this field"))
		}
	}
	return allErrs
}

// validateBootstrapIgnitionStaging checks the consent value for the
// ignition hook.
//
// Unset means Disabled. It is spelled as an explicit enum rather than a boolean
// because the field is a consent to hand a hook the cluster's day-0 secret
// material, and "bootstrapIgnitionStaging: false" reads like a feature being
// turned off rather than a permission being withheld.
//
// This only checks the value. Whether the payload actually declares an
// ignition hook is not knowable until the payload is pulled, so the
// pairing -- hook declared, consent absent -- is enforced at resolve time.
func validateBootstrapIgnitionStaging(v external.BootstrapIgnitionStaging, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	switch v {
	case "", external.BootstrapIgnitionStagingDisabled, external.BootstrapIgnitionStagingAllowed:
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath, v,
			[]string{string(external.BootstrapIgnitionStagingDisabled),
				string(external.BootstrapIgnitionStagingAllowed)}))
	}
	return allErrs
}

// validatePayloadPaths checks the three paths a cluster creator may still set.
//
// They are all relative, and containment is the only property decidable from
// the string alone -- so it is the only one decided here. Whether the payload
// has a file at the overriding path is checked where the extracted tree
// exists.
//
// The two overrides are an escape hatch for a payload whose layout is wrong and
// whose owner is not available to rebuild it; a conforming payload needs
// neither. capiInstallerPath is different in kind: it names where the payload
// is extracted, inside the install directory, and it exists because an operator
// may need to move that tree off a small or noexec filesystem.
func validatePayloadPaths(p *external.ClusterAPIProvider, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, f := range []struct {
		name string
		path string
		root string
	}{
		{"capiInstallerPath", p.CapiInstallerPath, "install directory"},
		{"infrastructureComponents", p.InfrastructureComponents, "provider payload"},
		{"manifestTemplates", p.ManifestTemplates, "provider payload"},
	} {
		if f.path == "" {
			continue
		}
		if _, err := filepath.Localize(f.path); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child(f.name), f.path,
				fmt.Sprintf("must be a relative path inside the %s", f.root)))
		}
	}
	return allErrs
}

// variableName is the set of names a template token can have, so that a value
// supplied here can actually be referenced by one.
var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateVariables checks the substitution values.
//
// These are the one field a conforming payload genuinely requires from the
// cluster creator: the payload ships the Cluster API templates, and the region,
// network and image references in them are not knowable at build time.
func validateVariables(p *external.ClusterAPIProvider, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
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
