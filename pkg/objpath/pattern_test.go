package objpath

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCompileErrors(t *testing.T) {
	bad := []string{
		"", "spec", "/", "/a//b", "/a~2", ".", ".spec[", `.spec["x`, ".spec[abc", ".spec[x=]",
		".a | foo", ".a | select(name == 1)", ".a | select(.b != 1)", ".a | select(.b == x)",
		".a[] | select(.b == 1", "| select(.a == 1)", ".a[x=1", ".a[x 1]", ".$", ".a.[0", ".a|",
	}
	for _, src := range bad {
		if _, err := Compile(src); err == nil {
			t.Errorf("Compile(%q) should fail", src)
		}
	}
}

func TestMustCompilePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	MustCompile("nope")
}

func sample() map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"labels":      map[string]any{"app": "web", "tier": "fe"},
			"annotations": map[string]any{"example.com/last-sync": "t", "keep": "k"},
		},
		"spec": map[string]any{
			"replicas": int64(3),
			"containers": []any{
				map[string]any{"name": "app", "image": "a:1", "resources": map[string]any{"requests": map[string]any{"cpu": "1"}}},
				map[string]any{"name": "side", "image": "s:1", "resources": map[string]any{"limits": map[string]any{"cpu": "1"}}},
				map[string]any{"name": "debug", "image": "d:1", "securityContext": map[string]any{"privileged": true}},
			},
		},
	}
}

func paths(ps []Path) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

func TestFindAndPrune(t *testing.T) {
	cases := []struct {
		src     string
		find    []string
		removed int
		check   func(t *testing.T, obj map[string]any)
	}{
		{src: "/spec/replicas", find: []string{".spec.replicas"}, removed: 1},
		{src: "/metadata/annotations/example.com~1last-sync", find: []string{`.metadata.annotations["example.com/last-sync"]`}, removed: 1},
		{src: "/spec/containers/1/image", find: []string{".spec.containers[1].image"}, removed: 1},
		{src: "/spec/containers/*/image", find: []string{".spec.containers[0].image", ".spec.containers[1].image", ".spec.containers[2].image"}, removed: 3},
		{src: ".spec.containers[].resources", find: []string{".spec.containers[0].resources", ".spec.containers[1].resources"}, removed: 2},
		{src: `.spec.containers[] | select(.name == "app") | .resources`, find: []string{".spec.containers[0].resources"}, removed: 1},
		{src: `.spec.containers[name=side].image`, find: []string{".spec.containers[1].image"}, removed: 1},
		{src: `.spec.containers[name="debug"]`, find: []string{".spec.containers[2]"}, removed: 1,
			check: func(t *testing.T, obj map[string]any) {
				if n := len(obj["spec"].(map[string]any)["containers"].([]any)); n != 2 {
					t.Fatalf("container not removed, %d left", n)
				}
			}},
		{src: `.spec.containers[] | select(.securityContext.privileged == true)`, find: []string{".spec.containers[2]"}, removed: 1},
		{src: `.spec.containers[] | select(.name == "app" and .image == "a:1") | .image`, find: []string{".spec.containers[0].image"}, removed: 1},
		{src: `.spec.containers[] | select(.missing == null) | .name`, find: []string{".spec.containers[0].name", ".spec.containers[1].name", ".spec.containers[2].name"}, removed: 3},
		{src: `.metadata.labels[]`, find: []string{".metadata.labels.app", ".metadata.labels.tier"}, removed: 2},
		{src: `.metadata["labels"].app`, find: []string{".metadata.labels.app"}, removed: 1},
		{src: `."metadata".labels`, find: []string{".metadata.labels"}, removed: 1},
		{src: `.spec.containers[0] | .image`, find: []string{".spec.containers[0].image"}, removed: 1},
		{src: `.spec.nothing`, find: nil, removed: 0},
		{src: `.spec.replicas.deeper`, find: nil, removed: 0},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			p, err := Compile(c.src)
			if err != nil {
				t.Fatal(err)
			}
			obj := sample()
			if got := paths(p.Find(obj)); !reflect.DeepEqual(got, c.find) && (len(got) != 0 || len(c.find) != 0) {
				t.Fatalf("Find = %v, want %v", got, c.find)
			}
			if n := p.Prune(obj); n != c.removed {
				t.Fatalf("Prune removed %d, want %d", n, c.removed)
			}
			if left := p.Find(obj); len(left) != 0 {
				t.Fatalf("matches remain after prune: %v", paths(left))
			}
			if c.check != nil {
				c.check(t, obj)
			}
		})
	}
}

func TestSyntaxAndString(t *testing.T) {
	p := MustCompile("  /spec/replicas ")
	if p.Syntax() != "json-pointer" || p.String() != "/spec/replicas" {
		t.Fatal(p.Syntax(), p.String())
	}
	if MustCompile(".spec").Syntax() != "jq" {
		t.Fatal("jq syntax")
	}
}

func TestRelates(t *testing.T) {
	obj := sample()
	app := Path{Field("spec"), Field("containers"), Key(KeyField{"name", "app"})}
	app[2].Index = 0
	side := Path{Field("spec"), Field("containers"), Key(KeyField{"name", "side"})}
	side[2].Index = 1
	sel := MustCompile(`.spec.containers[] | select(.name == "app") | .resources`)
	cases := []struct {
		p    *Pattern
		path Path
		want bool
	}{
		{sel, append(append(Path{}, app...), Field("resources"), Field("requests"), Field("cpu")), true}, // below a match
		{sel, append(append(Path{}, app...), Field("resources")), true},                                  // exact
		{sel, Path{Field("spec"), Field("containers")}, true},                                            // ancestor (atomic list)
		{sel, append(append(Path{}, side...), Field("resources")), false},                                // select fails
		{sel, append(append(Path{}, app...), Field("image")), false},                                     // other field
		{MustCompile("/spec/containers/1"), side, true},
		{MustCompile("/spec/containers/0"), side, false},
		{MustCompile("/spec/replicas"), Fields("spec", "replicas"), true},
		{MustCompile("/spec/0"), Fields("spec", "0"), true},
		{MustCompile(".spec.containers[1]"), Path{Field("spec"), Field("containers"), Index(1)}, true},
		{MustCompile(".spec.containers[name=side]"), side, true},
		{MustCompile(".spec.containers[name=side]"), app, false},
		{MustCompile(".metadata.labels"), Fields("spec"), false},
	}
	for i, c := range cases {
		if got := c.p.Relates(c.path, obj); got != c.want {
			t.Errorf("case %d: %s relates %s = %v, want %v", i, c.p, c.path, got, c.want)
		}
	}
}

func TestLiterals(t *testing.T) {
	obj := map[string]any{"items": []any{
		map[string]any{"n": int64(1), "f": 1.5, "b": false, "s": "x y"},
		map[string]any{"n": int64(2)},
	}}
	for src, want := range map[string]int{
		`.items[] | select(.n == 1)`:     1,
		`.items[] | select(.f == 1.5)`:   1,
		`.items[] | select(.b == false)`: 1,
		`.items[] | select(.s == "x y")`: 1,
		`.items[n=2]`:                    1,
		`.items[n==2]`:                   1,
		`.items[b=true]`:                 0,
		`.items[s=null]`:                 1,
	} {
		if got := len(MustCompile(src).Find(obj)); got != want {
			t.Errorf("%s: %d matches, want %d", src, got, want)
		}
	}
	if _, err := Compile(`.items[] | select(.s == "unterminated)`); err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("want unterminated string error, got %v", err)
	}
}

func FuzzCompile(f *testing.F) {
	for _, s := range []string{"/a/b", ".a.b[]", `.a[] | select(.x == "y") | .z`, ".a[k=v,j=1]", `.["x"]`, "/a/*/0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		p, err := Compile(src)
		if err != nil {
			return
		}
		obj := sample()
		_ = p.Relates(Fields("spec", "replicas"), obj)
		p.Prune(obj)
		if left := p.Find(obj); len(left) != 0 {
			t.Fatalf("%q: matches remain after prune: %v", src, paths(left))
		}
	})
}

func TestPositional(t *testing.T) {
	cases := []struct {
		src  string
		pos  bool
		segs []bool
	}{
		{"/spec/replicas", false, []bool{false, false}},
		{"/spec/containers/1/image", true, []bool{false, false, true, false}},
		{"/spec/containers/*/image", false, []bool{false, false, false, false}},
		{".spec.containers[0].image", true, []bool{false, false, true, false}},
		{`.spec.containers[] | select(.name == "app") | .env[2]`, true, []bool{false, false, false, false, true}},
		{".spec.containers[name=app].image", false, []bool{false, false, false, false}},
	}
	for _, c := range cases {
		p := MustCompile(c.src)
		if p.Positional() != c.pos || !reflect.DeepEqual(p.PositionalSegments(), c.segs) {
			t.Errorf("%s: positional %v segments %v", c.src, p.Positional(), p.PositionalSegments())
		}
		for _, f := range p.Find(sample()) {
			if len(f) != len(c.segs) {
				t.Errorf("%s: match %s has %d segments, want %d", c.src, f, len(f), len(c.segs))
			}
		}
	}
}

func TestSplitAtLastIndex(t *testing.T) {
	cases := []struct {
		src        string
		ok         bool
		head, tail int // step counts; tail -1 when nil
	}{
		{"/spec/replicas", false, 0, -1},
		{"/spec/containers/1", true, 3, -1},
		{"/spec/containers/1/args/*", true, 3, 2},
		{".spec.containers[0].env[1].value", true, 5, 1},
		{`.spec.containers[0] | select(.name == "web") | .args[]`, true, 4, 2},
		{`.spec.containers[0] | select(.name == "web")`, true, 4, -1},
	}
	for _, c := range cases {
		h, tl, ok := MustCompile(c.src).SplitAtLastIndex()
		if ok != c.ok {
			t.Errorf("%s: ok %v", c.src, ok)
			continue
		}
		if !ok {
			continue
		}
		if len(h.steps) != c.head || (tl == nil) != (c.tail < 0) || (tl != nil && len(tl.steps) != c.tail) {
			t.Errorf("%s: head %d tail %v", c.src, len(h.steps), tl)
		}
		if h.String() != MustCompile(c.src).String() {
			t.Errorf("%s: head source %q", c.src, h.String())
		}
	}
}

func TestPruneAt(t *testing.T) {
	obj := func() map[string]any {
		return map[string]any{"spec": map[string]any{
			"containers": []any{
				map[string]any{"name": "side", "args": []any{"p"}},
				map[string]any{"name": "web", "args": []any{"a", "b", "c"}, "m": []any{[]any{1, 2}}},
			},
		}}
	}
	web := append(Fields("spec", "containers"), Key(KeyField{Name: "name", Value: "web"}))
	cases := []struct {
		pat  string
		at   Path
		n    int
		want string
	}{
		{"/args/*", web, 3, `{"spec":{"containers":[{"args":["p"],"name":"side"},{"args":[],"m":[[1,2]],"name":"web"}]}}`},
		{".args[1]", web, 1, `{"spec":{"containers":[{"args":["p"],"name":"side"},{"args":["a","c"],"m":[[1,2]],"name":"web"}]}}`},
		// A list root: the shortened list is stored back in its parent.
		{"/0", append(append(Path(nil), web...), Field("m"), Index(0)), 1, `{"spec":{"containers":[{"args":["p"],"name":"side"},{"args":["a","b","c"],"m":[[2]],"name":"web"}]}}`},
		{"/args/*", append(Fields("spec", "containers"), Key(KeyField{Name: "name", Value: "nope"})), 0, ""},
		{"/nope", web, 0, ""},
	}
	for _, c := range cases {
		o := obj()
		if n := MustCompile(c.pat).PruneAt(o, c.at); n != c.n {
			t.Errorf("%s at %s: removed %d, want %d", c.pat, c.at, n, c.n)
			continue
		}
		b, _ := json.Marshal(o)
		if c.n == 0 {
			if !reflect.DeepEqual(o, obj()) {
				t.Errorf("%s at %s changed the object: %s", c.pat, c.at, b)
			}
			continue
		}
		if string(b) != c.want {
			t.Errorf("%s at %s: got %s", c.pat, c.at, b)
		}
	}
	// The whole object as the root.
	o := obj()
	if n := MustCompile("/spec/containers/1/args/0").PruneAt(o, nil); n != 1 {
		t.Errorf("root: removed %d", n)
	}
}

func TestSet(t *testing.T) {
	o := map[string]any{"a": map[string]any{"b": 1}, "l": []any{"x", map[string]any{"name": "k"}}}
	cases := []struct {
		p  Path
		ok bool
	}{
		{Fields("a", "b"), true},
		{Fields("a", "c"), false},
		{Fields("a", "b", "c"), false},
		{append(Fields("l"), Segment{Kind: SegValue, Value: "x"}), true},
		{append(Fields("l"), Segment{Kind: SegValue, Value: "x"}), false}, // replaced above
		{append(Fields("l"), Key(KeyField{Name: "name", Value: "k"})), true},
		{append(Fields("l"), Index(0)), true},
		{append(Fields("l"), Index(9)), false},
		{append(Fields("a"), Index(0)), false},
		{append(Fields("missing"), Index(0)), false},
	}
	for _, c := range cases {
		if got := set(o, c.p, "v"); got != c.ok {
			t.Errorf("set(%s) = %v", c.p, got)
		}
	}
}
