# Vornik Community Edition — injected at export time (the upstream Makefile
# carries operator/Enterprise build + deploy ops that don't ship publicly).
.PHONY: build test vet fmt lint docs-gen ui-css ui-css-check

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# lint: formatting + vet. The artifact-purity gates run via `go test`
# (internal/architecture) and in CI (.github/workflows/ci.yaml).
lint: vet
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

# docs-gen: regenerate the generated customer-docs pages (CLI/config reference +
# the CE/EE editions matrix) from source.
docs-gen:
	go run ./cmd/docs-gen all

# ui-css: regenerate the console stylesheet internal/ui/static/tailwind.css with
# the pinned standalone Tailwind CLI (fetched once, sha256-verified on every
# run). Run after changing a template's classes, a Go file under internal/ui
# that emits classes, or internal/ui/tailwind/; commit the output.
ui-css:
	@scripts/tailwind-css.sh

# ui-css-check: fail if the committed stylesheet is stale against its sources.
ui-css-check:
	@scripts/tailwind-css.sh --check
