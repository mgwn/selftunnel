.PHONY: fmt vet test smoke lint vuln coverage crosscompile build clean hooks

hooks:
	git config core.hooksPath scripts/hooks
	@echo "git hooks installed: commit-msg lint + pre-commit checks (scripts/hooks)"

# Pinned for reproducibility; bump deliberately and re-run make lint.
GOLANGCI_LINT := .gobin/golangci-lint

lint:
	@test -x $(GOLANGCI_LINT) || { echo "installing pinned golangci-lint v2.13.2 into .gobin (first run)"; GOBIN="$$(pwd)/.gobin" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2; }
	GOTOOLCHAIN=go1.26.8 $(GOLANGCI_LINT) run

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

coverage:
	bash tests/pipeline/coverage.sh

crosscompile:
	bash tests/pipeline/crosscompile.sh

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

smoke:
	bash tests/pipeline/smoke.sh

build: vet test
	./build.sh

clean:
	rm -rf dist selftunnel-server selftunnel-server-linux selftunnel-client selftunnel-client-gui
