package scenarios

import (
	"bytes"
	"strings"
	"testing"

	"github.com/archit2501/driftlens/internal/cluster"
	"github.com/archit2501/driftlens/pkg/drift"
	"github.com/archit2501/driftlens/pkg/manifest"
	"github.com/archit2501/driftlens/pkg/normalize"
	"github.com/archit2501/driftlens/pkg/report"
)

// defaultsCorpus covers every branch of the static defaulter.
const defaultsCorpus = `
apiVersion: v1
kind: Namespace
metadata: {name: defaults-check}
---
apiVersion: v1
kind: Pod
metadata: {name: pod, namespace: defaults-check}
spec:
  initContainers:
    - {name: init, image: "busybox@sha256:0000000000000000000000000000000000000000000000000000000000000000"}
  containers:
    - name: main
      image: registry.example.com:5000/team/app
      ports: [{containerPort: 8080}]
      livenessProbe: {grpc: {port: 9090}}
      startupProbe: {tcpSocket: {port: 8080}}
      lifecycle:
        preStop: {httpGet: {path: /stop, port: 8080}}
      env:
        - name: NODE
          valueFrom: {fieldRef: {fieldPath: spec.nodeName}}
  volumes:
    - {name: s, secret: {secretName: s}}
    - name: p
      projected: {sources: [{configMap: {name: c}}]}
    - name: d
      downwardAPI:
        items: [{path: labels, fieldRef: {fieldPath: metadata.labels}}]
    - {name: h, hostPath: {path: /var/log}}
    - {name: e, emptyDir: {}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: recreate, namespace: defaults-check}
spec:
  strategy: {type: Recreate}
  selector: {matchLabels: {app: r}}
  template:
    metadata: {labels: {app: r}}
    spec:
      containers: [{name: c, image: "nginx:latest"}]
---
apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: rs, namespace: defaults-check}
spec:
  selector: {matchLabels: {app: rs}}
  template:
    metadata: {labels: {app: rs}}
    spec:
      containers: [{name: c, image: nginx}]
---
apiVersion: v1
kind: ReplicationController
metadata: {name: rc, namespace: defaults-check}
spec:
  selector: {app: rc}
  template:
    metadata: {labels: {app: rc}}
    spec:
      containers: [{name: c, image: nginx:1.27}]
---
apiVersion: v1
kind: PodTemplate
metadata: {name: tpl, namespace: defaults-check}
template:
  spec:
    containers: [{name: c, image: nginx:1.27}]
---
apiVersion: batch/v1
kind: Job
metadata: {name: job, namespace: defaults-check}
spec:
  template:
    spec:
      restartPolicy: Never
      containers: [{name: c, image: busybox:1.36, command: ["true"]}]
---
apiVersion: v1
kind: Service
metadata: {name: lb, namespace: defaults-check}
spec:
  type: LoadBalancer
  selector: {app: x}
  ports: [{port: 443, targetPort: https}]
---
apiVersion: v1
kind: Service
metadata: {name: ext, namespace: defaults-check}
spec:
  type: ExternalName
  externalName: db.example.com
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: pvc, namespace: defaults-check}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 2Gi}}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: egress, namespace: defaults-check}
spec:
  podSelector: {}
  egress:
    - ports: [{port: 53, protocol: UDP}, {port: 443}]
---
apiVersion: v1
kind: Secret
metadata: {name: typed, namespace: defaults-check}
data: {k: dg==}
`

// TestStaticDefaultsMatchServer is the contract test for the static
// defaulter: for every object in the corpus, the statically defaulted object
// must compare InSync with what the API server returns from a dry-run create
// of the same object. Fields the server adds that the table lacks show up
// as live-only changes owned by the dry-run field manager.
func TestStaticDefaultsMatchServer(t *testing.T) {
	c := requireEnv(t)
	objs, err := manifest.Decode(strings.NewReader(defaultsCorpus), "corpus")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx(t), objs[0], "test-setup"); err != nil {
		t.Fatal(err)
	}
	server := cluster.ServerDefaulter{Client: c}
	eng := drift.New(drift.Options{Defaulter: normalize.NoDefaults{}})
	for _, o := range objs[1:] {
		t.Run(o.GetKind()+"/"+o.GetName(), func(t *testing.T) {
			want, err := server.Default(ctx(t), o)
			if err != nil {
				t.Fatal(err)
			}
			got, err := normalize.Static{}.Default(ctx(t), o)
			if err != nil {
				t.Fatal(err)
			}
			res := eng.Diff(ctx(t), got, want)
			if res.Status != drift.StatusInSync {
				var b bytes.Buffer
				rep := &drift.Report{Objects: []drift.ObjectResult{res}}
				rep.Summarize()
				_ = report.Text(&b, rep, report.TextOptions{ShowIgnored: true})
				t.Errorf("static defaults disagree with the API server:\n%s", b.String())
			}
		})
	}
}

// TestServerDefaultsScenarios runs every SSA scenario again with the
// server-side dry-run defaulter: the text report must equal the golden one
// produced with static defaults.
func TestServerDefaultsScenarios(t *testing.T) {
	c := requireEnv(t)
	for _, s := range scenarios(t) {
		t.Run(s.Name, func(t *testing.T) {
			if s.Spec.Apply == "csa" {
				t.Skip("client-side-apply scenarios cannot be set up twice")
			}
			if err := s.Setup(ctx(t), c); err != nil {
				t.Fatal(err)
			}
			rep, err := engineFor(s, cluster.ServerDefaulter{Client: c}).Run(ctx(t), s.Desired, c)
			if err != nil {
				t.Fatal(err)
			}
			txt, _ := render(t, rep)
			compareGolden(t, s.Dir, "expected.txt", txt, false)
		})
	}
}
