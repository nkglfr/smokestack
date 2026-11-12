# smokestack — build, test and release.
#
#   make                 build ./dist/smokestack for this machine
#   make test            vet + unit tests
#   make audit           gofmt, vet, staticcheck, govulncheck (needs audit-tools)
#   make audit-tools     install the two pinned analysers into $GOPATH/bin
#   make dist            signed release packages for linux amd64 + arm64
#   make release-key     create the release signing key pair (once)
#
# The official website and repository URLs are compiled into the binary:
#   make dist OFFICIAL_URL=https://smokestack.example.org
# (by default the official website is the GitHub repository)

VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
DATE         := $(shell date -u +%Y-%m-%d)
REPO_URL     ?= https://github.com/nkglfr/smokestack
OFFICIAL_URL ?= $(REPO_URL)
RELEASE_KEY  ?= release.key
ARCHS        ?= amd64 arm64

# The audit tools, pinned. A new release of either can turn the build red on
# a branch that changed nothing, and a red build nobody caused is a red build
# people learn to ignore. Bumped deliberately; the CI keys its tool cache on
# this file.
#
# Both require the go directive in go.mod to be at least 1.26.
GOVULNCHECK_VERSION ?= v1.8.0
STATICCHECK_VERSION ?= v0.8.1

# The toolchain the audit judges, named rather than inherited from the
# machine. govulncheck reports standard library vulnerabilities for the Go it
# runs under, so a developer on 1.27 would otherwise be told about the
# standard library of a toolchain no release is built with — in either
# direction, and silently. The CI matches go.mod by construction; naming the
# version here is what makes a local run say the same thing. It costs a
# toolchain download once on a machine that has a different Go, and nothing
# where it is already installed.
#
# The current patch of the 1.26 line. Behind by a patch it reports a standard
# library fix as still missing, which is a false alarm rather than a silence,
# so it is worth bumping but not urgent.
#
# 1.26.9 -> 1.26.10 after the Monday audit of 26 October went red on a
# crypto/x509 advisory in the standard library of 1.26.9. The releases
# themselves were never affected: setup-go asks for the latest patch, and
# `go version -m` on the published 0.7.3 binary reports go1.26.10, which
# carries the fix. That gap between what ships and what the audit judges is
# the whole reason this is a named version rather than whatever `go` the
# machine has.
#
# 1.26.10 -> 1.26.11 on GO-2026-6859, an HTTP/1 server crash from a
# MaxBytesReader header race in net/http. This one is not a false alarm and
# the gap ran the other way: the runner resolved "1.26" to a cached 1.26.10,
# so what ships was the affected version while only the audit would have been
# judged on a newer one. Bumping this alone would have turned the job green
# over a vulnerable build, which is worse than the red that found it. So the
# workflows now name the patch too — see the note in ci.yml.
AUDIT_TOOLCHAIN ?= go1.26.11

LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.BuildDate=$(DATE) \
           -X main.OfficialURL=$(OFFICIAL_URL) -X main.RepoURL=$(REPO_URL)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: build test audit audit-tools dist release-key check-branding clean install tidy

build:
	@mkdir -p dist
	$(GOBUILD) -o dist/smokestack .
	@./dist/smokestack version

tidy:
	go mod tidy

test:
	go vet ./...
	go test ./...

# What ships is the Linux build, so that is what is judged: ts_other.go is a
# stub, and analysing the host's GOOS on a Mac both skips the code every
# instance runs and reports a comparison in tracer.go that is only ever true
# there. Pinning GOOS is what makes a local run agree with the CI.
#
# gofmt is checked here rather than in `test` because it is a style gate, not
# a correctness one — but it is checked, which it never was: it was written
# down as mandatory and enforced nowhere.
audit: AUDIT = GOTOOLCHAIN=$(AUDIT_TOOLCHAIN) GOOS=linux
audit:
	@test -z "$$(gofmt -l .)" || { echo "not gofmt'd:"; gofmt -l .; exit 1; }
	$(AUDIT) go vet ./...
	$(AUDIT) staticcheck ./...
	$(AUDIT) govulncheck ./...

audit-tools:
	GOTOOLCHAIN=$(AUDIT_TOOLCHAIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	GOTOOLCHAIN=$(AUDIT_TOOLCHAIN) go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)

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
	sed 's#https://github.com/nkglfr/smokestack#$(REPO_URL)#g' install.sh > dist/install.sh
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
