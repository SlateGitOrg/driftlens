// Command driftlens-demo runs an offline, end-to-end demonstration: it starts
// a throwaway kube-apiserver (envtest), applies the example shop the way a
// GitOps controller would, makes the kind of out-of-band changes people and
// controllers make, and runs the real driftlens binary against it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/archit2501/driftlens/internal/cluster"
	"github.com/archit2501/driftlens/internal/kubeenv"
	"github.com/archit2501/driftlens/pkg/drift"
	"github.com/archit2501/driftlens/pkg/manifest"
)

func main() {
	bin := flag.String("driftlens", "bin/driftlens", "path to the driftlens binary")
	example := flag.String("example", "examples/shop", "example directory")
	flag.Parse()
	if err := run(context.Background(), *bin, *example); err != nil {
		fmt.Fprintln(os.Stderr, "demo failed:", err)
		os.Exit(1)
	}
}

func section(title string) {
	fmt.Printf("\n== %s %s\n", title, strings.Repeat("=", max(0, 72-len(title))))
}

func run(ctx context.Context, bin, example string) error {
	bin, err := filepath.Abs(bin)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "driftlens-demo-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()

	section("1. Start a throwaway API server (envtest: kube-apiserver + etcd, no nodes)")
	env, err := kubeenv.Start()
	if err != nil {
		return err
	}
	defer func() { _ = env.Stop() }()
	kc, err := env.Kubeconfig()
	if err != nil {
		return err
	}
	kubeconfig := filepath.Join(work, "kubeconfig")
	if err := os.WriteFile(kubeconfig, kc, 0o600); err != nil {
		return err
	}
	c, err := cluster.New(env.Config)
	if err != nil {
		return err
	}
	fmt.Println("API server up; KUBECONFIG points at it for every command below.")

	prod := filepath.Join(example, "overlays/prod")
	next := filepath.Join(example, "overlays/next")
	config := filepath.Join(example, "driftlens.yaml")
	d := &driver{bin: bin, kubeconfig: kubeconfig, work: work}

	section("2. GitOps sync: apply `kustomize build overlays/prod` as argocd-controller")
	objs, err := manifest.BuildKustomization(prod)
	if err != nil {
		return err
	}
	for _, o := range objs {
		if _, err := c.Apply(ctx, o, "argocd-controller"); err != nil {
			return err
		}
		fmt.Printf("applied %s\n", manifest.RefOf(o))
	}

	section("3. Right after the sync: everything is in sync")
	if err := d.run("diff", "-k", prod, "--config", config); err != nil {
		return err
	}

	section("4. Out-of-band changes during an incident")
	type change struct {
		say, manager, sub, name string
		patchType          types.PatchType
		patch              string
	}
	for _, ch := range []change{
		{"kubectl scale deployment/web --replicas=6", "kubectl-scale", "scale", "web", types.MergePatchType, `{"spec":{"replicas":6}}`},
		{"kubectl set image deployment/worker worker=ghcr.io/example/shop-worker:2.3.1-hotfix", "kubectl-set", "", "worker", types.JSONPatchType,
			`[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"ghcr.io/example/shop-worker:2.3.1-hotfix"}]`},
		{"kubectl edit deployment/worker   (adds --debug to args)", "kubectl-edit", "", "worker", types.JSONPatchType,
			`[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--debug"}]`},
		{"kubectl label deployment/web incident=INC-4211", "kubectl-label", "", "web", types.MergePatchType, `{"metadata":{"labels":{"incident":"INC-4211"}}}`},
		{"HPA controller scales deployment/api to 5 (kube-controller-manager, scale subresource)", "kube-controller-manager", "scale", "api", types.MergePatchType, `{"spec":{"replicas":5}}`},
		{"deploy bot stamps example.com/synced-at on deployment/api", "sync-bot", "", "api", types.MergePatchType, `{"metadata":{"annotations":{"example.com/synced-at":"2026-10-25T08:00:00Z"}}}`},
	} {
		ref := manifest.Ref{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "shop", Name: ch.name}
		if err := c.Patch(ctx, ref, ch.patchType, []byte(ch.patch), ch.manager, ch.sub); err != nil {
			return err
		}
		fmt.Printf("simulated: %s\n", ch.say)
	}

	section("5. Meanwhile Git moved on (overlays/next: worker 2.4.0, recommendations on)")
	fmt.Println("driftlens separates what Git changed from what the cluster changed:")
	if err := d.run("diff", "-k", next, "--config", config, "--show-ignored"); err != nil {
		return err
	}

	section("6. The same verdict as JSON (summary shown), for CI and bots")
	out, err := d.capture("diff", "-k", next, "--config", config, "-o", "json")
	if err != nil {
		return err
	}
	var rep drift.Report
	if err := json.Unmarshal(out, &rep); err != nil {
		return err
	}
	summary, err := json.MarshalIndent(rep.Summary, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(summary))

	section("7. Offline: snapshot the live objects, diff later without the cluster")
	snap := filepath.Join(work, "live.yaml")
	if err := d.run("snapshot", "-k", next, "--out", snap); err != nil {
		return err
	}
	offline, err := d.captureOffline("diff", "-k", next, "--config", config, "-o", "json", "--live", snap)
	if err != nil {
		return err
	}
	fmt.Printf("snapshot: %d bytes; offline JSON report identical to live report: %v\n", fileSize(snap), string(offline) == string(out))

	section("8. Exit codes for pipelines")
	for _, mode := range []string{"any", "drift", "never"} {
		code := d.exitCode("diff", "-k", next, "--config", config, "--exit-code", mode)
		fmt.Printf("driftlens diff --exit-code=%-5s -> exit %d\n", mode, code)
	}
	return nil
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return st.Size()
}

// driver runs the driftlens binary with KUBECONFIG pointing at envtest.
type driver struct {
	bin, kubeconfig, work string
}

// shown renders arguments with the temporary directory abbreviated.
func (d *driver) shown(args []string) string {
	return strings.ReplaceAll(strings.Join(args, " "), d.work, "$DEMO_TMP")
}

func (d *driver) cmd(args ...string) *exec.Cmd {
	cmd := exec.Command(d.bin, args...) // #nosec G204 -- the demo runs the binary it just built
	cmd.Env = append(os.Environ(), "KUBECONFIG="+d.kubeconfig, "NO_COLOR=1")
	return cmd
}

// run prints the command and its output; exit code 1 (drift) is expected.
func (d *driver) run(args ...string) error {
	fmt.Printf("$ driftlens %s\n", d.shown(args))
	cmd := d.cmd(args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		fmt.Println("(exit 1: drift found)")
		return nil
	}
	return err
}

func (d *driver) capture(args ...string) ([]byte, error) {
	fmt.Printf("$ driftlens %s\n", d.shown(args))
	out, err := d.cmd(args...).Output()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return out, nil
	}
	return out, err
}

// captureOffline runs without KUBECONFIG to prove no cluster is contacted.
func (d *driver) captureOffline(args ...string) ([]byte, error) {
	fmt.Printf("$ KUBECONFIG=/dev/null driftlens %s\n", d.shown(args))
	cmd := exec.Command(d.bin, args...) // #nosec G204 -- the demo runs the binary it just built
	cmd.Env = append(os.Environ(), "KUBECONFIG=/dev/null", "NO_COLOR=1")
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return out, nil
	}
	return out, err
}

func (d *driver) exitCode(args ...string) int {
	cmd := d.cmd(args...)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return -1
	}
	return 0
}
