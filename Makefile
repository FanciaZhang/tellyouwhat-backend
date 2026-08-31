.PHONY: test race vet build-gateway build-worker

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

build-gateway:
	go build ./cmd/gateway

build-worker:
	go build ./cmd/worker

