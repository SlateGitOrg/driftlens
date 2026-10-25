// Package kubeenv starts a throwaway kube-apiserver + etcd (controller-runtime
// envtest) for tests and the demo. No kubelet or controllers run, so only API
// semantics (validation, defaulting, field ownership) are real.
package kubeenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// K8sVersion is the envtest asset version the project is pinned to.
const K8sVersion = "1.37.0"

// ErrNoAssets is returned when no kube-apiserver/etcd binaries can be found.
var ErrNoAssets = errors.New("envtest binaries not found: run `make envtest` (or set KUBEBUILDER_ASSETS)")

// Assets returns the directory holding kube-apiserver and etcd: the
// KUBEBUILDER_ASSETS variable, else setup-envtest's default per-user cache.
func Assets() (string, error) {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir, check(dir)
	}
	base, err := envtest.SetupEnvtestDefaultBinaryAssetsDirectory()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoAssets, err)
	}
	dir := filepath.Join(base, fmt.Sprintf("%s-%s-%s", K8sVersion, runtime.GOOS, runtime.GOARCH))
	return dir, check(dir)
}

func check(dir string) error {
	for _, bin := range []string{"kube-apiserver", "etcd"} {
		if _, err := os.Stat(filepath.Join(dir, bin)); err != nil { // #nosec G703 -- operator-chosen asset directory
			return fmt.Errorf("%w (looked in %s)", ErrNoAssets, dir)
		}
	}
	return nil
}

// Env is a running API server.
type Env struct {
	Config *rest.Config
	te     *envtest.Environment
}

// Start launches the API server.
func Start() (*Env, error) {
	dir, err := Assets()
	if err != nil {
		return nil, err
	}
	te := &envtest.Environment{BinaryAssetsDirectory: dir}
	cfg, err := te.Start()
	if err != nil {
		return nil, fmt.Errorf("start envtest: %w", err)
	}
	return &Env{Config: cfg, te: te}, nil
}

// Kubeconfig returns a kubeconfig file body for a cluster-admin user.
func (e *Env) Kubeconfig() ([]byte, error) {
	u, err := e.te.AddUser(envtest.User{Name: "driftlens-demo", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		return nil, err
	}
	return u.KubeConfig()
}

// Stop shuts the API server down.
func (e *Env) Stop() error { return e.te.Stop() }
