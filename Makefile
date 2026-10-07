GO ?= go

.PHONY: build test race vet prose check evidence validate readme

build:
	$(GO) build -o bin/ ./cmd/...

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
