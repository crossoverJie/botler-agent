# botler-agent (Go) build/release helper.
#
# The version has a single source of truth: the git tag. `make release VERSION=x.y.z` tags and
# pushes; the GitHub Release workflow builds the cross-platform binaries from that tag.

BINARY  := botler
PKG     := ./cmd/botler
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "")
LDFLAGS := -s -w -X github.com/crossoverJie/botler-agent/internal/version.Version=$(VERSION) \
           -X github.com/crossoverJie/botler-agent/internal/version.Commit=$(COMMIT)

IMAGE ?= botler-agent
TAG   ?= dev

.PHONY: all build test lint fmt vet clean current check-clean release tag cover cover-html image

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

test:
	go test ./...

# Must match CI (.github/workflows/ci-go.yml): -coverpkg makes the number reflect the whole
# internal/ tree instead of each package's own tests only (runner.go is reached from dispatch's
# tests, so without it the number looks ~16 points worse than reality).
cover:
	go test -count=1 -covermode=atomic -coverpkg=./internal/... -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1

cover-html: cover
	go tool cover -html=coverage.out

fmt:
	gofmt -l -w .

vet:
	go vet ./...

# lint mirrors CI without requiring golangci-lint to be installed locally.
lint: fmt vet
	@test -z "$$(gofmt -l .)" || { echo "error: gofmt found unformatted files"; exit 1; }

# The image stamps VERSION/COMMIT into the binary, so build it from git describe rather than
# relying on the Dockerfile's "dev" fallback.
image:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE):$(TAG) .

clean:
	rm -f $(BINARY)
	rm -rf dist/

current:
	@echo $(VERSION)

check-clean:
	@git diff --quiet || { echo "error: working tree has uncommitted changes"; exit 1; }
	@git diff --cached --quiet || { echo "error: index has staged changes"; exit 1; }

release: check-clean
	@test -n "$(VERSION)" || { echo "error: pass VERSION=x.y.z (e.g. make release VERSION=0.6.0)"; exit 1; }
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' \
		|| { echo "error: VERSION must be semver (e.g. 0.6.0)"; exit 1; }
	git tag -a "v$(VERSION)" -m "release v$(VERSION)"
	git push origin "v$(VERSION)"

tag: release
