# gnopm. No build target on purpose: `go build` is already one word, and a
# target that only wraps it is a place for the flags to drift apart from CI's.
#
# make          the checks CI runs, which is what you want before pushing
# make test     the tests alone
# make lint     gofmt, vet, staticcheck
# make demo     the integration test, which is also the demo repository
# make install  put gnopm on your PATH
# make run      ARGS="status -json"

GO ?= go
STATICCHECK_VERSION ?= v0.7.0
DEMO_DIR ?= /tmp/gnopm-demo
ARGS ?= help

.PHONY: all
all: lint test demo

.PHONY: test
test:
	$(GO) test ./...

.PHONY: lint
lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "staticcheck not on PATH, skipping it."; \
		echo "  go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)"; \
		echo "  (needs a Go at least as new as the one it was built with; CI pins both)"; \
	fi

# The integration test is also the demo repository, deliberately the same
# thing: a demo that is not executed rots, one executed as a test cannot.
.PHONY: demo
demo:
	./scripts/demo.sh $(DEMO_DIR)

# Regenerates docs/img/ from real output. Run it when output changes; never
# hand-edit an SVG.
.PHONY: screenshots
screenshots:
	./scripts/screenshots.sh

.PHONY: install
install:
	$(GO) install ./...
	@command -v gnopm >/dev/null 2>&1 && gnopm version || \
		echo "installed, but gnopm is not on PATH: add $$($(GO) env GOPATH)/bin"

.PHONY: run
run:
	$(GO) run . $(ARGS)

.PHONY: clean
clean:
	rm -rf $(DEMO_DIR)
	$(GO) clean
