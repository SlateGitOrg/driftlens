# driftlens — every gate in one place. `make check` is what CI runs.
SHELL := /usr/bin/env bash
GO ?= go
export GOFLAGS ?= -p=3
GOLANGCI_LINT ?= golangci-lint
ENVTEST_K8S := 1.37.0
SETUP_ENVTEST := $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
COVERAGE_MIN := 80
BIN := bin/driftlens

.PHONY: all check fmt-check lint vet test cover build demo envtest fixtures golden bench fuzz clean

all: check

check: fmt-check vet lint test cover ## format-check + vet + lint + tests (incl. envtest) + coverage gate
	@echo "make check: OK"

fmt-check:
	@out=$$(gofmt -l $$(git ls-files '*.go')); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

lint:
	$(GOLANGCI_LINT) run ./...

envtest: ## download kube-apiserver + etcd once (needs network the first time)
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S) -p path >/dev/null

# envtest is mandatory in `make check`: DRIFTLENS_REQUIRE_ENVTEST makes a
# missing API server a failure instead of a skip.
test: envtest
	@KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S) -p path)" DRIFTLENS_REQUIRE_ENVTEST=1 \
	  $(GO) test -race -count=1 -json -coverpkg=./pkg/...,./internal/... -coverprofile=cover.out ./... > test-results.json; \
	  status=$$?; hack/test-count.sh test-results.json || status=1; \
	  if [ $$status -ne 0 ]; then grep -h '"Action":"output"' test-results.json | sed -E 's/.*"Output":"(.*)"}$$/\1/' | sed 's/\\n$$//; s/\\t/\t/g; s/\\"/"/g' | grep -E -- '--- FAIL|_test.go|panic' | head -50; fi; \
	  exit $$status

cover:
	@hack/coverage.sh cover.out $(COVERAGE_MIN)

build:
	$(GO) build -trimpath -ldflags "-X github.com/archit2501/driftlens/internal/cli.Version=$$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o $(BIN) ./cmd/driftlens

demo: build envtest ## offline end-to-end demo on a throwaway API server
	@KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S) -p path)" $(GO) run ./cmd/driftlens-demo --driftlens $(BIN)

fixtures: envtest ## re-capture live.yaml snapshots of every scenario from envtest
	@KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S) -p path)" DRIFTLENS_REQUIRE_ENVTEST=1 \
	  $(GO) test ./test/scenarios/ -run TestScenariosLive -count=1 -update-live -update

golden: ## rewrite expected.txt/expected.json from the offline replay
	$(GO) test ./test/scenarios/ -run TestGoldenOffline -count=1 -update

bench: ## engine throughput benchmarks
	$(GO) test ./pkg/drift/ -run '^$$' -bench . -benchmem -count=1

fuzz: ## short fuzzing session for the path-pattern parser
	$(GO) test ./pkg/objpath/ -run '^$$' -fuzz FuzzCompile -fuzztime 30s

clean:
	rm -rf bin cover.out test-results.json
