.PHONY: all build test clean build-ts test-ts clean-ts reset

all: build test

build: build-ts

test: test-ts

clean: clean-ts

# TypeScript
build-ts:
	npm run build

test-ts:
	npm test

clean-ts:
	rm -rf dist dist-test

reset:
	npm run reset
