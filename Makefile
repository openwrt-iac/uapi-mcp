BINARY  := uapi-mcp
VERSION ?= dev

.PHONY: build test fmt vet tidy gen smoke clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) .

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

# Regenerate internal/tools/catalog.json from internal/gen/openapi.json.
# Idempotent: re-run after bumping the vendored spec.
gen:
	go run ./internal/gen

# End-to-end smoke against a real router, driving the binary that make build
# just produced: UAPI_ENDPOINT, UAPI_TOKEN_FILE and optionally UAPI_INSECURE
# (see scripts/smoke.py). EXPECTED_TOOLS in the script moves with the vendored
# spec.
smoke: build
	UAPI_MCP_BINARY=./$(BINARY) python3 scripts/smoke.py

clean:
	rm -f $(BINARY)
