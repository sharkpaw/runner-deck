# Build artifacts
BINARY  := runner-manager
VERSION ?= dev

# Default tag for locally built runner image; when using the image pushed by CI, same repo name with tag suffixed -runner, e.g. ghcr.io/<owner>/<repo>:v2.0.0-runner
RUNNER_IMAGE ?= ghcr.io/sharkpaw/runner-deck:v2.0.0-runner

# GID of the docker group the app user joins inside the image; must match the host to access docker.sock (getent group docker | cut -d: -f3)
DOCKER_GID ?= 999

# Custom runner image example (examples/runner-images/Dockerfile.$(EXAMPLE))
EXAMPLE ?= android
IMAGE   ?= runner-deck-$(EXAMPLE)-runner:dev

.PHONY: build build-agent build-all test test-race test-coverage lint check install-ci-recipes run docker-build docker-build-runner docker-build-runner-example docker-run docker-stop clean help

help:
	@echo "targets: build build-agent build-all test test-race test-coverage lint check install-ci-recipes run docker-build docker-build-runner docker-build-runner-example docker-run docker-stop clean"
	@echo "  check: run this one before committing — it contains every check CI runs"
	@echo "  install-ci-recipes: install the ci-recipes that check needs (same version CI pins)"
	@echo "  docker-build-runner-example: build the custom runner image example, e.g."
	@echo "    make docker-build-runner-example EXAMPLE=android IMAGE=your-registry/android-runner:1"

COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(BUILD_DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/runner-manager

build-agent:
	go build -o runner-agent ./cmd/runner-agent

build-all: build build-agent

test:
	go test ./...

# Linux only; keep every production package in the statement coverage gate.
test-coverage:
	bash scripts/test-coverage.sh

# This is what CI runs. Concurrency surface (registration queue, runnerOps lock, single-winner creation of Agent tokens)
# Plain go test passes silently when these break, so run it at least once before committing.
test-race:
	go test -race ./...

# Corresponds to the Lint step of the Test job in CI. Scope is ./..., a bit larger than the union
# of the two workflows (./cmd/runner-manager/... ./internal/... and ./cmd/runner-agent/... ./internal/...),
# so anything that passes locally will pass CI.
#
# Version not pinned: CI uses version: latest, so pinning here would make the two sides judge differently.
GOLANGCI_LINT ?= golangci-lint

lint:
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		echo "$(GOLANGCI_LINT) not found. Install:"; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		echo 'After installing, make sure $$(go env GOPATH)/bin is on PATH.'; \
		exit 1; \
	}
	$(GOLANGCI_LINT) run --max-same-issues=100000 ./...

# The version-consistency and translation section-structure checks are provided by the runner-fleet recipe
# in soulteary/ci-recipes; this repo only supplies scripts/ci-recipes.conf.
#
# Versions are not rewritten here; they are read from the workflow. If two places each pin a version and one is changed without the other, local and CI will judge differently,
# which is exactly the gap lint originally left open. If the version can't be read, check fails immediately
# rather than proceeding with an empty version.
CI_RECIPES          ?= ci-recipes
CI_RECIPES_WORKFLOW := .github/workflows/ci-consistency.yml
CI_RECIPES_VERSION  ?= $(shell sed -n 's/^[[:space:]]*CI_RECIPES_VERSION:[[:space:]]*"\([^"]*\)".*/\1/p' $(CI_RECIPES_WORKFLOW))

# Run this one before pushing. It covers every check CI runs, so you don't have to rebuild the list from memory.
# The item you forget is always the one that passes locally and fails in CI (last time it was lint).
check:
	@out=$$(gofmt -l ./cmd ./internal); \
	if [ -n "$$out" ]; then \
		echo "The following files are not formatted. Run gofmt -w ./cmd ./internal:"; \
		echo "$$out"; \
		exit 1; \
	fi
	go vet ./...
	$(MAKE) lint
	$(MAKE) test-coverage
	@command -v $(CI_RECIPES) >/dev/null 2>&1 || { \
		echo "$(CI_RECIPES) not found. Install with: make install-ci-recipes"; \
		echo 'After installing, confirm $$(go env GOPATH)/bin is on PATH.'; \
		exit 1; \
	}
	$(CI_RECIPES) runner-fleet check-version-consistency
	$(CI_RECIPES) runner-fleet check-docs-structure
	@echo "All checks passed"

run: build
	./$(BINARY)

docker-build:
	docker build --build-arg VERSION=$(VERSION) --build-arg DOCKER_GID=$(DOCKER_GID) -t runner-manager:$(VERSION) .

docker-build-runner:
	docker build -f Dockerfile.runner --build-arg DOCKER_GID=$(DOCKER_GID) -t $(RUNNER_IMAGE) .

# Adds the toolchain on top of this repo's runner image. See examples/runner-images/README.md
docker-build-runner-example:
	docker build -f examples/runner-images/Dockerfile.$(EXAMPLE) -t $(IMAGE) .

# If Basic Auth is needed, use docker compose (it reads .env), or add -e BASIC_AUTH_PASSWORD=... -e BASIC_AUTH_USER=admin to this target's docker run
docker-run: docker-stop
	docker run -d --name runner-manager -p 8080:8080 \
		-v $(PWD)/config:/app/config \
		-v $(PWD)/runners:/app/runners \
		runner-manager:$(VERSION)

docker-stop:
	-docker stop runner-manager 2>/dev/null; docker rm runner-manager 2>/dev/null; true

# Installs the ci-recipes that the checks need. The version is read from the workflow, so it doesn't have to be repeated here, in the docs, or in CI
# version number. Hardcoding it would go stale, and the version-consistency check would flag that vX.Y.Z as a reference to this repo
# version reference (it only matches literals and can't tell it's another module's version).
install-ci-recipes:
	@[ -n "$(CI_RECIPES_VERSION)" ] || { \
		echo "Could not read CI_RECIPES_VERSION from $(CI_RECIPES_WORKFLOW). Refusing to install with an empty version."; \
		exit 1; \
	}
	go install "github.com/soulteary/ci-recipes/cmd/ci-recipes@$(CI_RECIPES_VERSION)"
	@echo "Installed. Confirm $$(go env GOPATH)/bin is on PATH."

clean:
	rm -f $(BINARY) runner-agent
