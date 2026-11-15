.PHONY: build test vet
build:
	go build -o bin/swarm ./cmd/swarm
test:
	go test ./... -count=1
vet:
	go vet ./...
