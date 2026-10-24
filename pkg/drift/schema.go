package drift

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/client-go/applyconfigurations"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/structured-merge-diff/v6/typed"
)

const (
	schemaOpenAPI = "openapi"
	schemaDeduced = "deduced"
)

// typer converts unstructured objects to structured-merge-diff typed values
// so that comparisons understand associative lists (containers keyed by
// name, ports keyed by port+protocol, ...). Built-in kinds use the OpenAPI
// schema compiled into client-go; anything else (CRDs, kinds newer than this
// build) falls back to the "deduced" schema, where every list is atomic.
type typer struct {
	tc managedfields.TypeConverter
}

func newTyper() *typer {
	return &typer{tc: applyconfigurations.NewTypeConverter(scheme.Scheme)}
}

// convert types every non-nil object with the same schema. If any object
// fails schema validation, all of them use the deduced schema, and the
// validation error is returned as a warning.
func (t *typer) convert(objs ...*unstructured.Unstructured) ([]*typed.TypedValue, string, string, error) {
	out := make([]*typed.TypedValue, len(objs))
	var warn string
	ok := true
	for i, o := range objs {
		if o == nil {
			continue
		}
		if !scheme.Scheme.Recognizes(o.GroupVersionKind()) {
			ok = false
			break
		}
		tv, err := t.tc.ObjectToTyped(o, typed.AllowDuplicates)
		if err != nil {
			ok = false
			warn = fmt.Sprintf("schema validation failed, comparing with deduced schema (lists are atomic): %v", err)
			break
		}
		out[i] = tv
	}
	if ok {
		return out, schemaOpenAPI, "", nil
	}
	for i, o := range objs {
		if o == nil {
			out[i] = nil
			continue
		}
		tv, err := typed.DeducedParseableType.FromUnstructured(o.Object, typed.AllowDuplicates)
		if err != nil {
			return nil, "", warn, fmt.Errorf("convert object: %w", err)
		}
		out[i] = tv
	}
	return out, schemaDeduced, warn, nil
}
