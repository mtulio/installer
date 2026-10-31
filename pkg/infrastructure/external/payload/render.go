package payload

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// tokenPattern matches clusterctl's substitution syntax:
//
//	${VAR}              required; no value is an error
//	${VAR=default}      default applies when VAR is unset
//	${VAR:=default}     default applies when VAR is unset or empty
//	${VAR:="default"}   as above, with the surrounding quotes stripped
//
// This is deliberately the same dialect every provider's existing
// cluster-template*.yaml already uses, so a partner maintains one template
// language across their upstream and their OpenShift artifacts rather than
// learning a second one. Go templates and kustomize were both rejected for
// that reason, and kustomize additionally cannot express "fail if this value
// is unset", which is the behaviour below.
//
// A default value containing a closing brace is not supported. No provider
// template in either pilot uses one, and the alternative is a hand-written
// parser for a case nobody has.
var tokenPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:?=)?([^}]*)\}`)

// InstallerVariablePrefix marks the variables the installer supplies itself.
// The prefix keeps the installer's namespace distinct from the provider's
// inside the same file, so the two can grow independently; adding a variable
// is compatible, renaming one is not.
const InstallerVariablePrefix = "INSTALLER_"

// token is one parsed occurrence.
type token struct {
	name       string
	hasDefault bool
	def        string
}

func parseToken(m []string) token {
	t := token{name: m[1]}
	if m[2] != "" {
		t.hasDefault = true
		t.def = strings.Trim(m[3], `"`)
	}
	return t
}

// RequiredVariables returns the names of every token used under dir that has
// no default, sorted and deduplicated.
//
// It exists so the installer can refuse an incomplete configuration during
// validation, before any cloud call and before the controller starts, rather
// than at the point of rendering. That is where every other install-config
// error surfaces, and it matches clusterctl's own behaviour on an unset
// required variable.
func RequiredVariables(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	seen := map[string]struct{}{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !isTemplateFile(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range tokenPattern.FindAllStringSubmatch(string(data), -1) {
			if t := parseToken(m); !t.hasDefault {
				seen[t.name] = struct{}{}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to scan %s for template variables: %w", dir, err)
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// isTemplateFile reports whether a file is rendered.
//
// Only YAML is. Hooks in particular are copied verbatim and never rendered:
// a hook script expands ${OPENSHIFT_INSTALL_INFRA_ID} and friends as *shell*
// variables at run time, and substituting them at render time would destroy
// the script.
func isTemplateFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// Render substitutes vars into every YAML file under src and writes the result
// under dst, preserving the relative layout. Non-YAML files are copied
// unchanged.
//
// An unresolvable token -- used, no default, no value supplied -- is an error
// naming every missing variable at once rather than the first. A user filling
// in one value per failed install is a user running the installer five times
// to learn five things it already knew.
func Render(src, dst string, vars map[string]string) error {
	if src == "" {
		return nil
	}
	var missing []string
	seenMissing := map[string]struct{}{}

	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out := data
		if isTemplateFile(path) {
			rendered, miss := substitute(string(data), vars)
			for _, name := range miss {
				if _, ok := seenMissing[name]; !ok {
					seenMissing[name] = struct{}{}
					missing = append(missing, name)
				}
			}
			out = []byte(rendered)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, out, info.Mode().Perm())
	})
	if err != nil {
		return fmt.Errorf("failed to render %s into %s: %w", src, dst, err)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("the provider payload's templates use variables that nothing resolves: %s. "+
			"Supply them in platform.external.clusterAPI.variables, or use a payload whose templates declare "+
			"defaults for them", strings.Join(missing, ", "))
	}
	return nil
}

// substitute applies the token rules to one document, returning the result and
// the names of any token that could not be resolved. Unresolved tokens are
// left in place: the caller turns them into an error, and leaving the original
// text makes a partially rendered file readable if one is ever written for
// debugging.
func substitute(in string, vars map[string]string) (string, []string) {
	var missing []string
	out := tokenPattern.ReplaceAllStringFunc(in, func(match string) string {
		m := tokenPattern.FindStringSubmatch(match)
		t := parseToken(m)
		value, ok := vars[t.name]
		switch {
		case ok && value != "":
			return value
		case ok && value == "":
			// An explicitly empty value is a value for ${VAR} and ${VAR=d},
			// and is treated as unset only by the ":=" form -- which is the
			// whole difference between the two spellings upstream.
			if t.hasDefault && strings.HasPrefix(m[2], ":") {
				return t.def
			}
			return ""
		case t.hasDefault:
			return t.def
		default:
			missing = append(missing, t.name)
			return match
		}
	})
	return out, missing
}
