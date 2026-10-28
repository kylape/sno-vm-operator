.PHONY: test build

test:
	go test -race ./...
	go vet ./...

build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/manager ./cmd/manager
