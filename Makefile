GO ?= go

.PHONY: build linux test race vet prose check evidence validate readme compose-smoke

build:
	$(GO) build -o bin/ ./cmd/...

# Static Linux binaries for the container image, for the architecture Docker
# runs here.
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(shell $(GO) env GOARCH) $(GO) build -o bin/linux/ ./cmd/...

compose-smoke:
	./script/compose_smoke.sh

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

prose:
	ruby script/check_prose.rb

check: vet prose test

evidence: build
	$(GO) run ./cmd/evidence -out evidence/results

validate:
	$(GO) run ./script/validate_evidence evidence/results
	$(GO) run ./script/readme_numbers -check

readme:
	$(GO) run ./script/readme_numbers
