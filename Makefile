.PHONY: bootstrap build test race functional
ENGINE_IMAGE ?= registry.dagger.io/engine:v1.0.0-beta.25

bootstrap:
	sh scripts/bootstrap.sh

build: bootstrap
	mkdir -p bin
	GOMAXPROCS=4 go build -p 2 -o bin/dcache ./cmd/dcache

test: bootstrap
	GOMAXPROCS=4 go test -p 2 -count=1 ./...

race: bootstrap
	GOMAXPROCS=4 go test -race -p 2 -count=1 ./...

functional: build
	python3 tests/functional/run.py --binary "$$(pwd)/bin/dcache" --output "$$(pwd)/artifacts/functional" --engine-image "$(ENGINE_IMAGE)" --scenario all
