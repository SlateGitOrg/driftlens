package cluster

import (
	"strings"
	"testing"
)

func TestGeneratePrefix(t *testing.T) {
	cases := map[string]string{
		"web":                            "web-dl-",
		"9lives":                         "dl9lives-dl-",
		"Web":                            "web-dl-",
		strings.Repeat("a", 60):          strings.Repeat("a", 40) + "-dl-",
		strings.Repeat("a", 39) + "-xyz": strings.Repeat("a", 39) + "-dl-",
	}
	for in, want := range cases {
		if got := generatePrefix(in); got != want {
			t.Errorf("generatePrefix(%q) = %q, want %q", in, got, want)
		}
		if len(generatePrefix(in))+5 > 63 {
			t.Errorf("prefix for %q too long", in)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load("/nonexistent/kubeconfig", ""); err == nil {
		t.Fatal("expected error")
	}
}
