GO ?= go

.PHONY: build test race vet prose check evidence validate

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
	$(GO) run ./script/validate_evidence.go evidence/results
