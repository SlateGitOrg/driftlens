package normalize

import (
	"context"
	"reflect"
	"testing"
)

func defaulted(t *testing.T, y string) map[string]any {
	t.Helper()
	out, err := Static{}.Default(context.Background(), obj(t, y))
	if err != nil {
		t.Fatal(err)
	}
	// Idempotency is a property of every defaulter.
	again, err := Static{}.Default(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Object, again.Object) {
		t.Fatalf("defaulting is not idempotent:\n%v\n%v", out.Object, again.Object)
	}
	return out.Object
}

func at(m map[string]any, path ...any) any {
	var cur any = m
	for _, p := range path {
		switch k := p.(type) {
		case string:
			cur = cur.(map[string]any)[k]
		case int:
			cur = cur.([]any)[k]
		}
	}
	return cur
}

func TestDeploymentDefaults(t *testing.T) {
	o := defaulted(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: d}
spec:
  template:
    spec:
      containers:
        - name: c
          image: nginx
          ports: [{containerPort: 80}]
          readinessProbe: {httpGet: {port: 80}}
          lifecycle: {postStart: {grpc: {port: 1}}}
          env: [{name: N, valueFrom: {fieldRef: {fieldPath: spec.nodeName}}}]
      volumes:
        - {name: v, configMap: {name: x}}
        - {name: h, hostPath: {path: /x}}
`)
	checks := map[string][2]any{
		"replicas":       {at(o, "spec", "replicas"), int64(1)},
		"strategy":       {at(o, "spec", "strategy", "rollingUpdate", "maxSurge"), "25%"},
		"history":        {at(o, "spec", "revisionHistoryLimit"), int64(10)},
		"deadline":       {at(o, "spec", "progressDeadlineSeconds"), int64(600)},
		"restart":        {at(o, "spec", "template", "spec", "restartPolicy"), "Always"},
		"pull":           {at(o, "spec", "template", "spec", "containers", 0, "imagePullPolicy"), "Always"},
		"protocol":       {at(o, "spec", "template", "spec", "containers", 0, "ports", 0, "protocol"), "TCP"},
		"probe":          {at(o, "spec", "template", "spec", "containers", 0, "readinessProbe", "failureThreshold"), int64(3)},
		"scheme":         {at(o, "spec", "template", "spec", "containers", 0, "readinessProbe", "httpGet", "scheme"), "HTTP"},
		"grpc":           {at(o, "spec", "template", "spec", "containers", 0, "lifecycle", "postStart", "grpc", "service"), ""},
		"fieldRef":       {at(o, "spec", "template", "spec", "containers", 0, "env", 0, "valueFrom", "fieldRef", "apiVersion"), "v1"},
		"mode":           {at(o, "spec", "template", "spec", "volumes", 0, "configMap", "defaultMode"), int64(420)},
		"hostPath":       {at(o, "spec", "template", "spec", "volumes", 1, "hostPath", "type"), ""},
		"scheduler":      {at(o, "spec", "template", "spec", "schedulerName"), "default-scheduler"},
		"terminationMsg": {at(o, "spec", "template", "spec", "containers", 0, "terminationMessagePolicy"), "File"},
	}
	for name, c := range checks {
		if !reflect.DeepEqual(c[0], c[1]) {
			t.Errorf("%s: got %#v want %#v", name, c[0], c[1])
		}
	}
	recreate := defaulted(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\nspec: {strategy: {type: Recreate}}\n")
	if _, ok := at(recreate, "spec", "strategy").(map[string]any)["rollingUpdate"]; ok {
		t.Fatal("Recreate strategy got rollingUpdate")
	}
}

func TestPullPolicy(t *testing.T) {
	for img, want := range map[string]string{
		"nginx":                      "Always",
		"nginx:latest":               "Always",
		"nginx:1.27":                 "IfNotPresent",
		"localhost:5000/app":         "Always",
		"localhost:5000/app:v2":      "IfNotPresent",
		"repo/app@sha256:abcd":       "IfNotPresent",
		"registry.example.com/a/b:1": "IfNotPresent",
	} {
		if got := pullPolicy(img); got != want {
			t.Errorf("pullPolicy(%q) = %s, want %s", img, got, want)
		}
	}
}

func TestOtherKindDefaults(t *testing.T) {
	cases := []struct {
		name, y string
		path    []any
		want    any
	}{
		{"pod links", "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec: {containers: [{name: c, image: x}]}\n", []any{"spec", "enableServiceLinks"}, true},
		{"podtemplate", "apiVersion: v1\nkind: PodTemplate\nmetadata: {name: p}\ntemplate: {spec: {containers: [{name: c, image: x}]}}\n", []any{"template", "spec", "dnsPolicy"}, "ClusterFirst"},
		{"rc selector", "apiVersion: v1\nkind: ReplicationController\nmetadata: {name: r}\nspec: {template: {metadata: {labels: {a: b}}, spec: {containers: [{name: c, image: x}]}}}\n", []any{"spec", "selector", "a"}, "b"},
		{"rc labels", "apiVersion: v1\nkind: ReplicationController\nmetadata: {name: r}\nspec: {template: {metadata: {labels: {a: b}}}}\n", []any{"metadata", "labels", "a"}, "b"},
		{"rs", "apiVersion: apps/v1\nkind: ReplicaSet\nmetadata: {name: r}\nspec: {template: {spec: {}}}\n", []any{"spec", "replicas"}, int64(1)},
		{"sts partition", "apiVersion: apps/v1\nkind: StatefulSet\nmetadata: {name: s}\nspec: {template: {spec: {}}, volumeClaimTemplates: [{metadata: {name: d}, spec: {}}]}\n", []any{"spec", "updateStrategy", "rollingUpdate", "partition"}, int64(0)},
		{"sts claims", "apiVersion: apps/v1\nkind: StatefulSet\nmetadata: {name: s}\nspec: {template: {spec: {}}, volumeClaimTemplates: [{metadata: {name: d}, spec: {}}]}\n", []any{"spec", "volumeClaimTemplates", 0, "spec", "volumeMode"}, "Filesystem"},
		{"ds", "apiVersion: apps/v1\nkind: DaemonSet\nmetadata: {name: d}\nspec: {template: {spec: {}}}\n", []any{"spec", "updateStrategy", "rollingUpdate", "maxSurge"}, int64(0)},
		{"job completions", "apiVersion: batch/v1\nkind: Job\nmetadata: {name: j}\nspec: {template: {spec: {}}}\n", []any{"spec", "completions"}, int64(1)},
		{"job no restart", "apiVersion: batch/v1\nkind: Job\nmetadata: {name: j}\nspec: {template: {spec: {}}}\n", []any{"spec", "template", "spec", "restartPolicy"}, nil},
		{"job parallel", "apiVersion: batch/v1\nkind: Job\nmetadata: {name: j}\nspec: {parallelism: 3, template: {spec: {}}}\n", []any{"spec", "completions"}, nil},
		{"job pfp", "apiVersion: batch/v1\nkind: Job\nmetadata: {name: j}\nspec: {podFailurePolicy: {rules: []}, template: {spec: {}}}\n", []any{"spec", "podReplacementPolicy"}, nil},
		{"cronjob", "apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: c}\nspec: {jobTemplate: {spec: {template: {spec: {containers: [{name: c, image: x}]}}}}}\n", []any{"spec", "jobTemplate", "spec", "template", "spec", "containers", 0, "terminationMessagePath"}, "/dev/termination-log"},
		{"svc lb", "apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {type: LoadBalancer, ports: [{port: 80}]}\n", []any{"spec", "allocateLoadBalancerNodePorts"}, true},
		{"svc target", "apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {ports: [{port: 80}]}\n", []any{"spec", "ports", 0, "targetPort"}, int64(80)},
		{"svc external", "apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {type: ExternalName}\n", []any{"spec", "internalTrafficPolicy"}, nil},
		{"pvc", "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: p}\nspec: {}\n", []any{"spec", "volumeMode"}, "Filesystem"},
		{"secret", "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\n", []any{"type"}, "Opaque"},
		{"netpol egress", "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: n}\nspec: {egress: [{ports: [{port: 53}]}]}\n", []any{"spec", "policyTypes", 1}, "Egress"},
		{"netpol port", "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: n}\nspec: {egress: [{ports: [{port: 53}]}]}\n", []any{"spec", "egress", 0, "ports", 0, "protocol"}, "TCP"},
		{"configmap untouched", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\n", []any{"spec"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := defaulted(t, c.y)
			got := lookup(o, c.path)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v want %#v", got, c.want)
			}
		})
	}
}

// lookup is at() that tolerates missing keys (returns nil).
func lookup(m map[string]any, path []any) any {
	var cur any = m
	for _, p := range path {
		switch k := p.(type) {
		case string:
			mm, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = mm[k]
		case int:
			l, ok := cur.([]any)
			if !ok || k >= len(l) {
				return nil
			}
			cur = l[k]
		}
	}
	return cur
}

func TestMalformedInputIsLeftAlone(t *testing.T) {
	for _, y := range []string{
		"apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\nspec: {strategy: broken, template: 5}\n",
		"apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\n",
		"apiVersion: apps/v1\nkind: StatefulSet\nmetadata: {name: d}\n",
		"apiVersion: apps/v1\nkind: DaemonSet\nmetadata: {name: d}\n",
		"apiVersion: batch/v1\nkind: Job\nmetadata: {name: d}\n",
		"apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: d}\n",
		"apiVersion: v1\nkind: Service\nmetadata: {name: d}\n",
		"apiVersion: v1\nkind: ReplicationController\nmetadata: {name: d}\n",
		"apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: d}\n",
		"apiVersion: v1\nkind: Pod\nmetadata: {name: d}\nspec: {containers: [5, {name: c}]}\n",
	} {
		defaulted(t, y)
	}
}
