// Package cli implements the driftlens command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/SlateGitOrg/driftlens/internal/cluster"
	"github.com/SlateGitOrg/driftlens/pkg/drift"
	"github.com/SlateGitOrg/driftlens/pkg/manifest"
	"github.com/SlateGitOrg/driftlens/pkg/normalize"
	"github.com/SlateGitOrg/driftlens/pkg/report"
	"github.com/SlateGitOrg/driftlens/pkg/rules"
)

// Version is set at build time with -ldflags "-X .../internal/cli.Version=...".
var Version = "dev"

// Exit codes.
const (
	ExitOK    = 0 // no drift (or --exit-code=never)
	ExitDrift = 1 // drift found
	ExitError = 2 // usage or runtime error
)

// IO bundles the process streams so commands are testable.
type IO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
	// IsTerminal reports whether Out is a terminal (for --color=auto).
	IsTerminal bool
	// RESTConfig, when set, is used instead of loading a kubeconfig.
	RESTConfig *rest.Config
}

const usage = `driftlens — semantic drift between Git desired state and live Kubernetes objects

Usage:
  driftlens diff     [-f PATH]... [-k DIR]... [--live FILE | --kubeconfig FILE] [flags]
  driftlens snapshot [-f PATH]... [-k DIR]... [--kubeconfig FILE] [--out FILE]
  driftlens rules    [--config FILE]
  driftlens version

Run "driftlens <command> -h" for the flags of a command.

Exit codes: 0 no drift, 1 drift found, 2 error.
`

// Run executes the command line and returns the process exit code.
func Run(ctx context.Context, args []string, sio IO) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(sio.Err, usage)
		return ExitError
	}
	var err error
	code := ExitOK
	switch args[0] {
	case "diff":
		code, err = runDiff(ctx, args[1:], sio)
	case "snapshot":
		err = runSnapshot(ctx, args[1:], sio)
	case "rules":
		err = runRules(args[1:], sio)
	case "version", "--version":
		_, err = fmt.Fprintf(sio.Out, "driftlens %s\n", Version)
	case "help", "-h", "--help":
		_, err = fmt.Fprint(sio.Out, usage)
	default:
		err = usageError(fmt.Sprintf("unknown command %q", args[0]))
	}
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if err != nil {
		_, _ = fmt.Fprintf(sio.Err, "driftlens: %v\n", err)
		var ue usageError
		if errors.As(err, &ue) {
			_, _ = fmt.Fprint(sio.Err, "\n"+usage)
		}
		return ExitError
	}
	return code
}

type usageError string

func (u usageError) Error() string { return string(u) }

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// sourceFlags are shared by diff and snapshot.
type sourceFlags struct {
	files, kustomize    multi
	kubeconfig, kubeCtx string
	namespace           string
	verbose             bool
}

func (s *sourceFlags) register(fs *flag.FlagSet) {
	fs.Var(&s.files, "f", "manifest file or directory, or - for stdin (repeatable)")
	fs.Var(&s.files, "filename", "same as -f")
	fs.Var(&s.kustomize, "k", "kustomization directory to build in-process (repeatable)")
	fs.Var(&s.kustomize, "kustomize", "same as -k")
	fs.StringVar(&s.kubeconfig, "kubeconfig", "", "kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&s.kubeCtx, "context", "", "kubeconfig context")
	fs.StringVar(&s.namespace, "n", "default", "namespace for namespaced objects that set none")
	fs.StringVar(&s.namespace, "namespace", "default", "same as -n")
	fs.BoolVar(&s.verbose, "v", false, "debug logging to stderr")
}

func (s *sourceFlags) desired(sio IO) ([]*unstructured.Unstructured, error) {
	if len(s.files) == 0 && len(s.kustomize) == 0 {
		return nil, usageError("no desired state: pass -f and/or -k")
	}
	objs, err := manifest.Load(s.files, sio.In)
	if err != nil {
		return nil, err
	}
	for _, k := range s.kustomize {
		ko, err := manifest.BuildKustomization(k)
		if err != nil {
			return nil, err
		}
		objs = append(objs, ko...)
	}
	return objs, nil
}

func (s *sourceFlags) client(sio IO) (*cluster.Client, error) {
	cfg := sio.RESTConfig
	if cfg == nil {
		var err error
		cfg, err = cluster.Load(s.kubeconfig, s.kubeCtx)
		if err != nil {
			return nil, err
		}
	}
	return cluster.New(cfg)
}

func (s *sourceFlags) logger(sio IO) *slog.Logger {
	level := slog.LevelWarn
	if s.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(sio.Err, &slog.HandlerOptions{Level: level}))
}

func runDiff(ctx context.Context, args []string, sio IO) (int, error) {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(sio.Err)
	var (
		src                              sourceFlags
		live                             multi
		configFile, output, defaults     string
		color, exitCode                  string
		showIgnored, showInSync, explain bool
	)
	src.register(fs)
	fs.Var(&live, "live", "live-state snapshot file (repeatable); compare offline instead of querying a cluster")
	fs.StringVar(&configFile, "config", "", "driftlens configuration file (DriftConfig)")
	fs.StringVar(&output, "o", "text", "output format: text or json")
	fs.StringVar(&output, "output", "text", "same as -o")
	fs.StringVar(&defaults, "defaults", "static", "how to fill API defaults into desired: static, server (dry-run create) or none")
	fs.StringVar(&color, "color", "auto", "colorize text output: auto, always or never")
	fs.StringVar(&exitCode, "exit-code", "any", "when to exit 1: any (any difference), drift (live drift, missing or errors only), never")
	fs.BoolVar(&showIgnored, "show-ignored", false, "list differences hidden by ignore rules")
	fs.BoolVar(&showInSync, "show-in-sync", false, "list objects without differences too")
	fs.BoolVar(&explain, "explain", false, "print why each difference was attributed to Git or live")
	if err := fs.Parse(args); err != nil {
		return ExitError, err
	}
	if fs.NArg() > 0 {
		return ExitError, usageError("unexpected arguments: " + strings.Join(fs.Args(), " "))
	}
	if output != "text" && output != "json" {
		return ExitError, usageError("-o must be text or json")
	}
	if color != "auto" && color != "always" && color != "never" {
		return ExitError, usageError("--color must be auto, always or never")
	}
	if exitCode != "any" && exitCode != "drift" && exitCode != "never" {
		return ExitError, usageError("--exit-code must be any, drift or never")
	}
	cfg := rules.Default()
	if configFile != "" {
		var err error
		if cfg, err = rules.Load(configFile); err != nil {
			return ExitError, err
		}
	}
	desired, err := src.desired(sio)
	if err != nil {
		return ExitError, err
	}

	var source drift.LiveSource
	var client *cluster.Client
	if len(live) > 0 {
		snap, err := manifest.Load(live, sio.In)
		if err != nil {
			return ExitError, err
		}
		source = drift.NewSnapshotSource(snap)
	} else {
		if client, err = src.client(sio); err != nil {
			return ExitError, err
		}
		source = client
	}
	var defaulter normalize.Defaulter
	switch defaults {
	case "static":
		defaulter = normalize.Static{}
	case "none":
		defaulter = normalize.NoDefaults{}
	case "server":
		if client == nil {
			return ExitError, usageError("--defaults=server needs a cluster connection (not --live)")
		}
		defaulter = cluster.ServerDefaulter{Client: client}
	default:
		return ExitError, usageError("--defaults must be static, server or none")
	}

	eng := drift.New(drift.Options{Config: cfg, Defaulter: defaulter, DefaultNamespace: src.namespace, Logger: src.logger(sio)})
	rep, err := eng.Run(ctx, desired, source)
	if err != nil {
		return ExitError, err
	}
	if output == "json" {
		err = report.JSON(sio.Out, rep)
	} else {
		useColor := color == "always" || (color == "auto" && sio.IsTerminal && os.Getenv("NO_COLOR") == "")
		err = report.Text(sio.Out, rep, report.TextOptions{Color: useColor, ShowIgnored: showIgnored, ShowInSync: showInSync, Explain: explain})
	}
	if err != nil {
		return ExitError, err
	}
	switch {
	case exitCode == "never":
		return ExitOK, nil
	case exitCode == "drift" && rep.HasLiveDrift():
		return ExitDrift, nil
	case exitCode == "any" && rep.HasDrift():
		return ExitDrift, nil
	}
	return ExitOK, nil
}

func runSnapshot(ctx context.Context, args []string, sio IO) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fs.SetOutput(sio.Err)
	var src sourceFlags
	var out string
	src.register(fs)
	fs.StringVar(&out, "out", "-", "write the snapshot to this file (- for stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	desired, err := src.desired(sio)
	if err != nil {
		return err
	}
	client, err := src.client(sio)
	if err != nil {
		return err
	}
	objs, err := client.Snapshot(ctx, desired, src.namespace)
	if err != nil {
		return err
	}
	b, err := manifest.EncodeList(objs)
	if err != nil {
		return err
	}
	if out == "-" {
		_, err = sio.Out.Write(b)
		return err
	}
	return os.WriteFile(out, b, 0o600)
}

func runRules(args []string, sio IO) error {
	fs := flag.NewFlagSet("rules", flag.ContinueOnError)
	fs.SetOutput(sio.Err)
	var configFile string
	fs.StringVar(&configFile, "config", "", "configuration file to validate (default: built-in)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := rules.Default()
	if configFile != "" {
		var err error
		if cfg, err = rules.Load(configFile); err != nil {
			return err
		}
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := sio.Out.Write(b); err != nil {
		return err
	}
	for _, r := range cfg.Ignore {
		for _, p := range r.Patterns() {
			if _, err := fmt.Fprintf(sio.Out, "# %s: %s (%s)\n", r.Name, p, p.Syntax()); err != nil {
				return err
			}
		}
	}
	return nil
}
