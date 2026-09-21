# smokestack — build, test and release.
#
#   make                 build ./dist/smokestack for this machine
#   make test            vet + unit tests
#   make dist            signed release packages for linux amd64 + arm64
#   make release-key     create the release signing key pair (once)
#
# The official website and repository URLs are compiled into the binary:
#   make dist OFFICIAL_URL=https://smokestack.example.org REPO_URL=https://github.com/acme/smokestack

VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
DATE         := $(shell date -u +%Y-%m-%d)
OFFICIAL_URL ?= https://smokestack.CHANGE-ME
REPO_URL     ?= https://github.com/CHANGE-ME/smokestack
RELEASE_KEY  ?= release.key
ARCHS        ?= amd64 arm64

LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.BuildDate=$(DATE) \
           -X main.OfficialURL=$(OFFICIAL_URL) -X main.RepoURL=$(REPO_URL)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: build test dist release-key check-branding clean install tidy

build:
	@mkdir -p dist
	$(GOBUILD) -o dist/smokestack .
	@./dist/smokestack version

tidy:
	go mod tidy

test:
	go vet ./...
	go test ./...

# Refuses to publish with placeholder URLs: they would end up in the
# footer of every instance and break automatic updates.
check-branding:
	@case "$(OFFICIAL_URL)$(REPO_URL)" in *CHANGE-ME*) \
	  echo "error: set OFFICIAL_URL and REPO_URL (make dist OFFICIAL_URL=... REPO_URL=...)"; exit 1;; esac
	@case "$(VERSION)" in *dirty*|*-g*) \
	  echo "error: releases are built from a clean tag (git tag v1.2.3), got $(VERSION)"; exit 1;; esac

dist: check-branding test
	@rm -rf dist && mkdir -p dist
	@for arch in $(ARCHS); do \
	  echo "==> linux/$$arch"; \
	  GOOS=linux GOARCH=$$arch $(GOBUILD) -o dist/smokestack-linux-$$arch . || exit 1; \
	  go run . release pack -binary dist/smokestack-linux-$$arch -version $(VERSION) \
	     -os linux -arch $$arch $(if $(SMOKESTACK_RELEASE_KEY),,-key $(RELEASE_KEY)) \
	     -out dist/smokestack-$(VERSION)-linux-$$arch.zip || exit 1; \
	done
	go run . release latest -version $(VERSION) \
	   -base-url $(REPO_URL)/releases/download/v$(VERSION) \
	   -notes-url $(REPO_URL)/releases/tag/v$(VERSION) \
	   dist/smokestack-$(VERSION)-linux-*.zip > dist/latest.json
	sed 's#https://github.com/CHANGE-ME/smokestack#$(REPO_URL)#g' install.sh > dist/install.sh
	cd dist && sha256sum *.zip install.sh latest.json > SHA256SUMS
	@rm -f dist/smokestack-linux-*
	@echo "==> release files in ./dist"; ls -1 dist

release-key:
	go run . release keygen -out .
	@echo "Append the public key line above to release.pub, commit it,"
	@echo "and store the content of release.key as the CI secret SMOKESTACK_RELEASE_KEY."

install: build
	sudo ./install.sh --package dist/smokestack

clean:
	rm -rf dist
