# Build, test and publish the TypeScript (ts/), Go (go/) and Rust (rs/)
# implementations. ts/ is canonical; go/ and rs/ track it.
#
# TypeScript and Go build against the published @tabnas siblings (npm, the
# Go proxy); admin/scripts/link.sh can point them at local checkouts instead
# (node_modules symlinks + a go.work one level up). rs/ takes the engine
# crate by path from a sibling checkout.

.PHONY: all build test clean build-ts build-go build-rs test-ts test-go test-rs \
        clean-ts clean-go clean-rs downstream publish-ts publish-go version-rs \
        tags-go reset prose prose-counts

all: build test

build: build-ts build-go build-rs

test: test-ts test-go test-rs

clean: clean-ts clean-go clean-rs

# Run the front-end suites against this working tree, in TypeScript, Go and
# Rust. Required before any emit-pipeline change is done, and before a
# release: a green build here proves much less than usual, because what
# this package emits is executed elsewhere. See scripts/downstream.sh and
# AGENTS.md, "Verify your work".
#
#   make downstream                    # abnf ebnf gbnf, every runtime
#   make downstream PEERS="gbnf"       # just that one
#   make downstream RUNTIMES="ts go"   # without the Rust half
downstream:
	RUNTIMES="$(RUNTIMES)" ./scripts/downstream.sh $(PEERS)

# --- TypeScript (package in ts/) ---
build-ts:
	cd ts && npm run build

test-ts:
	cd ts && npm test

clean-ts:
	rm -rf ts/dist ts/dist-test

# Publish the TypeScript package at its current package.json version.
publish-ts: test-ts
	cd ts && npm publish --access public

# --- Go (module in go/) ---
build-go:
	cd go && go build ./...

test-go:
	cd go && go test -v ./...

clean-go:
	cd go && go clean

# Publish the Go module: make publish-go V=x.y.z
# Injects V into the Go `VERSION` const, commits, tags go/vX.Y.Z, and
# (when gh is available) creates a GitHub release.
publish-go: test-go
	@test -n "$(V)" || (echo "Usage: make publish-go V=x.y.z" && exit 1)
	@grep -q '^const VERSION = ' go/bnf.go || \
	  (echo "publish-go: no 'const VERSION = ' in go/bnf.go — refusing to tag a release with an unbumped constant" && exit 1)
	sed -i.bak 's/^const VERSION = ".*"/const VERSION = "$(V)"/' go/bnf.go
	rm -f go/bnf.go.bak
	git add go/bnf.go
	git commit -m "go: v$(V)"
	git tag go/v$(V)
	git push origin main go/v$(V)
	@command -v gh >/dev/null 2>&1 && gh release create go/v$(V) --title "go/v$(V)" --notes "Go module release v$(V)" || true

# --- Rust (crate in rs/) ---
build-rs:
	cd rs && cargo build --all-targets

# `--all-targets` excludes the doctests, and the README's examples run as
# doctests, so both test lines are needed.
test-rs:
	cd rs && cargo test --all-targets && cargo test --doc
	cd rs && cargo clippy --all-targets --all-features -- -D warnings

clean-rs:
	cd rs && cargo clean

# Set the Rust crate version: make version-rs V=x.y.z
#
# Bumps BOTH Rust version sites, plus the crate's own entry in
# rs/Cargo.lock, which rs/tests/version_test.rs holds to
# ts/package.json. A release that bumps the TS and Go sites and forgets
# these fails that test.
#
# Unlike publish-go it neither commits nor tags, and it publishes nothing:
# release.yml's crates job publishes tabnas-bnf to crates.io from the
# release tag, after crates-release.yml rewrites the manifest's path
# dependency as a crates.io requirement. Here only the constants need to
# stay in step.
version-rs:
	@test -n "$(V)" || (echo "Usage: make version-rs V=x.y.z" && exit 1)
	sed -i.bak 's/^version = ".*"/version = "$(V)"/' rs/Cargo.toml
	sed -i.bak 's/^pub const VERSION: &str = ".*";/pub const VERSION: \&str = "$(V)";/' rs/src/lib.rs
	rm -f rs/Cargo.toml.bak rs/src/lib.rs.bak
	cd rs && cargo metadata --format-version 1 --offline >/dev/null

# List published Go module tags, newest first.
tags-go:
	git tag -l 'go/v*' --sort=-version:refname

reset:
	cd ts && npm run reset
	cd go && go clean -cache && go build ./... && go test -v ./...

# The prose gate (see docs/STYLE-GUIDE.md). Vale over the reader-facing
# pages, at the levels set in .vale.ini, on the same file list
# ts/test/docs.test.js reads. Requires `vale` on PATH and one
# `vale sync`. Warnings are advisory, errors fail.
prose:
	vale --minAlertLevel=error $$(node ts/scripts/gated-docs.cjs)
	node ts/scripts/vale-counts.cjs

# Re-measure what .vale.ini and the style guide record, after
# a change to the pages or to the rules moves the numbers.
prose-counts:
	node ts/scripts/vale-counts.cjs --write
