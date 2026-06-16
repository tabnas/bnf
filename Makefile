# Build, test and publish the TypeScript package (ts/).
# This package has no Go port.

.PHONY: all build test clean build-ts test-ts clean-ts publish-ts reset

all: build test

build: build-ts

test: test-ts

clean: clean-ts

build-ts:
	cd ts && npm run build

test-ts:
	cd ts && npm test

clean-ts:
	rm -rf ts/dist ts/dist-test

# Publish the TypeScript package at its current package.json version.
publish-ts: test-ts
	cd ts && npm publish --access public

reset:
	cd ts && npm run reset
