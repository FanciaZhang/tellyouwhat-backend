.PHONY: test race vet generate-api verify-generated swift-client build-gateway build-worker build-admin

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

generate-api:
	go generate ./internal/httpapi ./internal/journalhttpapi ./internal/adminhttpapi ./internal/workerhttpapi

verify-generated: generate-api
	git diff --exit-code -- internal/httpapi/generated.go internal/journalhttpapi/generated.go internal/adminhttpapi/generated.go internal/workerhttpapi/generated.go

swift-client:
	swift build --package-path Contracts/HTTP

build-gateway:
	go build ./cmd/gateway

build-worker:
	go build ./cmd/worker

build-admin:
	go build ./cmd/admin
