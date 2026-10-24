// Package rules loads and validates the driftlens configuration: which field
// managers represent the GitOps pipeline, which look like humans, and which
// differences to ignore.
package rules

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"

	"sigs.k8s.io/yaml"

	"github.com/archit2501/driftlens/pkg/manifest"
	"github.com/archit2501/driftlens/pkg/objpath"
)

// ConfigKind is the kind written at the top of a configuration file.
const ConfigKind = "DriftConfig"

// ConfigAPIVersion is the only supported configuration version.
const ConfigAPIVersion = "driftlens/v1"

// DefaultGitOpsManagers are field managers treated as "the GitOps pipeline".
var DefaultGitOpsManagers = []string{
	"driftlens", "argocd-controller", "argocd-application-controller",
	"kustomize-controller", "helm-controller", "kubectl-client-side-apply",
}

// DefaultManualManagers are field managers treated as a human at a terminal
// or dashboard. "kubectl-client-side-apply" is excluded by the GitOps list,
// which always takes precedence.
var DefaultManualManagers = []string{"kubectl", "kubectl-*", "k9s", "Mozilla", "Lens", "kubernetes-dashboard"}

// Match selects objects. Empty fields match anything; values are globs.
type Match struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

// Matches reports whether the object reference is selected.
func (m Match) Matches(r manifest.Ref) bool {
	return glob(m.Group, r.Group) && glob(m.Kind, r.Kind) && glob(m.Namespace, r.Namespace) && glob(m.Name, r.Name)
}

func glob(pattern, s string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	ok, err := path.Match(pattern, s)
	return err == nil && ok
}

// IgnoreRule hides differences. Paths are removed from desired, live and
// last-applied objects before comparing; Managers hides every field owned by
// a matching field manager on the live object.
type IgnoreRule struct {
	Name     string   `json:"name,omitempty"`
	Match    Match    `json:"match,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Managers []string `json:"managers,omitempty"`

	patterns []*objpath.Pattern
}

// Patterns returns the compiled path patterns (after Validate).
func (r *IgnoreRule) Patterns() []*objpath.Pattern { return r.patterns }

// Config is the driftlens configuration file.
type Config struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	// GitOpsManagers are globs naming the field managers of the GitOps
	// pipeline. Fields they own are "ours".
	GitOpsManagers []string `json:"gitopsManagers,omitempty"`
	// ManualManagers are globs naming managers that indicate a manual edit.
	ManualManagers []string `json:"manualManagers,omitempty"`
	// Ignore lists ignore rules.
	Ignore []IgnoreRule `json:"ignore,omitempty"`
	// ReportUnattributed reports live-only fields even when no ownership or
	// last-applied information says who set them (they are usually server
	// defaults, so this is off by default).
	ReportUnattributed bool `json:"reportUnattributed,omitempty"`
}

// Default returns the built-in configuration.
func Default() *Config {
	c := &Config{
		APIVersion:     ConfigAPIVersion,
		Kind:           ConfigKind,
		GitOpsManagers: append([]string(nil), DefaultGitOpsManagers...),
		ManualManagers: append([]string(nil), DefaultManualManagers...),
	}
	if err := c.Validate(); err != nil {
		panic(err) // built-in configuration is static
	}
	return c
}

// Load reads a configuration file; omitted manager lists take defaults.
func Load(file string) (*Config, error) {
	b, err := os.ReadFile(file) // #nosec G304 -- user-supplied config path
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes and validates a configuration document.
func Parse(b []byte) (*Config, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return Default(), nil
	}
	c := &Config{}
	if err := yaml.UnmarshalStrict(b, c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.APIVersion != ConfigAPIVersion || c.Kind != ConfigKind {
		return nil, fmt.Errorf("config: want apiVersion %q and kind %q, got %q/%q", ConfigAPIVersion, ConfigKind, c.APIVersion, c.Kind)
	}
	if c.GitOpsManagers == nil {
		c.GitOpsManagers = append([]string(nil), DefaultGitOpsManagers...)
	}
	if c.ManualManagers == nil {
		c.ManualManagers = append([]string(nil), DefaultManualManagers...)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate compiles patterns and checks globs.
func (c *Config) Validate() error {
	var errs []error
	checkGlobs := func(where string, gs []string) {
		for _, g := range gs {
			if g == "" {
				errs = append(errs, fmt.Errorf("%s: empty manager name", where))
				continue
			}
			if _, err := path.Match(g, ""); err != nil {
				errs = append(errs, fmt.Errorf("%s: bad glob %q: %w", where, g, err))
			}
		}
	}
	checkGlobs("gitopsManagers", c.GitOpsManagers)
	checkGlobs("manualManagers", c.ManualManagers)
	for i := range c.Ignore {
		r := &c.Ignore[i]
		where := fmt.Sprintf("ignore[%d]", i)
		if r.Name != "" {
			where += " (" + r.Name + ")"
		}
		if len(r.Paths) == 0 && len(r.Managers) == 0 {
			errs = append(errs, fmt.Errorf("%s: needs paths or managers", where))
		}
		checkGlobs(where+".managers", r.Managers)
		for _, g := range []string{r.Match.Group, r.Match.Kind, r.Match.Namespace, r.Match.Name} {
			if _, err := path.Match(g, ""); err != nil {
				errs = append(errs, fmt.Errorf("%s.match: bad glob %q: %w", where, g, err))
			}
		}
		r.patterns = r.patterns[:0]
		for _, p := range r.Paths {
			pat, err := objpath.Compile(p)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", where, err))
				continue
			}
			r.patterns = append(r.patterns, pat)
		}
		if r.Name == "" {
			r.Name = fmt.Sprintf("ignore[%d]", i)
		}
	}
	return errors.Join(errs...)
}

// RulesFor returns the ignore rules that apply to an object.
func (c *Config) RulesFor(r manifest.Ref) []*IgnoreRule {
	var out []*IgnoreRule
	for i := range c.Ignore {
		if c.Ignore[i].Match.Matches(r) {
			out = append(out, &c.Ignore[i])
		}
	}
	return out
}

// IsGitOps reports whether a field manager belongs to the GitOps pipeline.
func (c *Config) IsGitOps(manager string) bool { return MatchGlobs(c.GitOpsManagers, manager) }

// IsManual reports whether a field manager indicates a manual edit.
func (c *Config) IsManual(manager string) bool {
	return !c.IsGitOps(manager) && MatchGlobs(c.ManualManagers, manager)
}

// MatchGlobs reports whether s matches any glob (path.Match syntax).
func MatchGlobs(globs []string, s string) bool {
	for _, g := range globs {
		if glob(g, s) {
			return true
		}
	}
	return false
}
