.PHONY: build test vet schema
build:
	go build -o bin/swarm ./cmd/swarm
test:
	go test ./... -count=1
vet:
	go vet ./...
schema:
	go run ./cmd/swarm schema > configs/swarm.schema.json
