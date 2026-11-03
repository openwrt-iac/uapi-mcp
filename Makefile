BINARY  := uapi-mcp
VERSION ?= dev

.PHONY: build test fmt vet tidy gen clean

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

clean:
	rm -f $(BINARY)
