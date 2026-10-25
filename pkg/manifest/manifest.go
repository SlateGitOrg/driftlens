// Package manifest loads desired-state Kubernetes objects from plain YAML or
// JSON files, directories, standard input (for example `kustomize build`
// output) and Kustomize directories, which are built in-process.
package manifest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

// Ref identifies an object independently of the API version it was read in.
type Ref struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// RefOf returns the reference of an object.
func RefOf(u *unstructured.Unstructured) Ref {
	gvk := u.GroupVersionKind()
	return Ref{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Namespace: u.GetNamespace(), Name: u.GetName()}
}

// GVK returns the group/version/kind of the reference.
func (r Ref) GVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: r.Group, Version: r.Version, Kind: r.Kind}
}

// Key identifies the object without its version: two refs with the same key
// name the same stored object.
func (r Ref) Key() string {
	return r.Group + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name
}

// String renders "apps/v1 Deployment default/web" (or "v1 Namespace shop").
func (r Ref) String() string {
	gv := r.Version
	if r.Group != "" {
		gv = r.Group + "/" + r.Version
	}
	id := r.Name
	if r.Namespace != "" {
		id = r.Namespace + "/" + r.Name
	}
	return gv + " " + r.Kind + " " + id
}

// Less orders refs by group, kind, namespace and name.
func (r Ref) Less(o Ref) bool {
	if r.Group != o.Group {
		return r.Group < o.Group
	}
	if r.Kind != o.Kind {
		return r.Kind < o.Kind
	}
	if r.Namespace != o.Namespace {
		return r.Namespace < o.Namespace
	}
	return r.Name < o.Name
}

// Decode reads a stream of YAML documents (or a single JSON document) and
// returns the objects in it. Documents of kind List (or any *List kind with
// an items array) are expanded. Empty documents are skipped. Numbers decode
// as int64 when integral, matching what the API server returns.
func Decode(r io.Reader, source string) ([]*unstructured.Unstructured, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(r))
	var out []*unstructured.Unstructured
	for doc := 1; ; doc++ {
		raw, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, doc, err)
		}
		if len(bytes.TrimSpace(stripComments(raw))) == 0 {
			continue
		}
		js, err := yaml.YAMLToJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, doc, err)
		}
		if bytes.Equal(bytes.TrimSpace(js), []byte("null")) {
			continue
		}
		var obj map[string]any
		if err := utiljson.Unmarshal(js, &obj); err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, doc, err)
		}
		objs, err := expand(obj)
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, doc, err)
		}
		out = append(out, objs...)
	}
}

func stripComments(b []byte) []byte {
	var keep [][]byte
	for _, line := range bytes.Split(b, []byte("\n")) {
		if t := bytes.TrimSpace(line); len(t) > 0 && t[0] != '#' {
			keep = append(keep, line)
		}
	}
	return bytes.Join(keep, []byte("\n"))
}

func expand(obj map[string]any) ([]*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{Object: obj}
	if items, ok := obj["items"].([]any); ok && strings.HasSuffix(u.GetKind(), "List") {
		var out []*unstructured.Unstructured
		for i, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("items[%d] is not an object", i)
			}
			sub, err := expand(m)
			if err != nil {
				return nil, fmt.Errorf("items[%d]: %w", i, err)
			}
			out = append(out, sub...)
		}
		return out, nil
	}
	if err := validate(u); err != nil {
		return nil, err
	}
	return []*unstructured.Unstructured{u}, nil
}

func validate(u *unstructured.Unstructured) error {
	switch {
	case u.GetAPIVersion() == "":
		return errors.New("object has no apiVersion")
	case u.GetKind() == "":
		return errors.New("object has no kind")
	case u.GetName() == "":
		return fmt.Errorf("%s has no metadata.name (generateName is not supported for drift detection)", u.GetKind())
	}
	if _, err := schema.ParseGroupVersion(u.GetAPIVersion()); err != nil {
		return err
	}
	return nil
}

// IsKustomization reports whether dir contains a kustomization file.
func IsKustomization(dir string) bool {
	for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if st, err := os.Stat(filepath.Join(dir, n)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// BuildKustomization runs `kustomize build` in-process on dir. Plugins are
// disabled and load restrictions are kustomize's default (local files must
// live under the kustomization root); remote bases are resolved by kustomize
// itself, as with the kustomize CLI.
func BuildKustomization(dir string) ([]*unstructured.Unstructured, error) {
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rm, err := k.Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	y, err := rm.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	return Decode(bytes.NewReader(y), "kustomize build "+dir)
}

// Load reads objects from each path. "-" reads stdin. A directory with a
// kustomization file is built with Kustomize; any other directory is walked
// recursively for .yaml, .yml and .json files (sorted, deterministic).
func Load(paths []string, stdin io.Reader) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, p := range paths {
		objs, err := loadOne(p, stdin)
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
	return out, nil
}

func loadOne(p string, stdin io.Reader) ([]*unstructured.Unstructured, error) {
	if p == "-" {
		return Decode(stdin, "<stdin>")
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return loadFile(p)
	}
	if IsKustomization(p) {
		return BuildKustomization(p)
	}
	var files []string
	err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != p && IsKustomization(path) {
				return fmt.Errorf("%s is a kustomization inside %s: pass it directly with -k", path, p)
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []*unstructured.Unstructured
	for _, f := range files {
		objs, err := loadFile(f)
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
	return out, nil
}

func loadFile(p string) ([]*unstructured.Unstructured, error) {
	f, err := os.Open(p) // #nosec G304 -- reading user-named manifest files is the purpose
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return Decode(f, p)
}

// EncodeList renders objects as a YAML v1 List, the format `driftlens
// snapshot` writes and `--live` reads.
func EncodeList(objs []*unstructured.Unstructured) ([]byte, error) {
	items := make([]any, 0, len(objs))
	for _, o := range objs {
		items = append(items, o.Object)
	}
	return yaml.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
}

// Dedupe reports objects that appear more than once (same Ref key).
func Dedupe(objs []*unstructured.Unstructured) ([]*unstructured.Unstructured, []Ref) {
	seen := map[string]bool{}
	var out []*unstructured.Unstructured
	var dups []Ref
	for _, o := range objs {
		r := RefOf(o)
		if seen[r.Key()] {
			dups = append(dups, r)
			continue
		}
		seen[r.Key()] = true
		out = append(out, o)
	}
	return out, dups
}
