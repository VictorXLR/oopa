BINARY ?= oopa
WEB_ADDR ?= 127.0.0.1:7777
GO ?= go

.PHONY: help build run web test race vet fmt check clean

help:
	@printf '%s\n' 'Targets:'
	@printf '%s\n' '  make build   Build ./$(BINARY)'
	@printf '%s\n' '  make run     Run the TUI'
	@printf '%s\n' '  make web     Run the web UI on $(WEB_ADDR)'
	@printf '%s\n' '  make test    Run tests'
	@printf '%s\n' '  make race    Run tests with the race detector'
	@printf '%s\n' '  make vet     Run go vet'
	@printf '%s\n' '  make fmt     Format Go files'
	@printf '%s\n' '  make check   Run fmt, vet, test, and race'
	@printf '%s\n' '  make clean   Remove built binary'

build:
	$(GO) build -o $(BINARY) .

run:
	$(GO) run .

web:
	$(GO) run . web $(WEB_ADDR)

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

check: fmt vet test race

clean:
	rm -f $(BINARY)
