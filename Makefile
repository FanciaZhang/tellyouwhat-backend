.PHONY: test race vet generate-api verify-generated swift-client build-gateway build-worker build-admin

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

generate-api:
	go generate ./internal/platformhttpapi ./internal/healthhttpapi ./internal/journalhttpapi ./internal/birthdayhttpapi ./internal/adminhttpapi ./internal/workerhttpapi ./internal/lifecyclehttpapi

verify-generated: generate-api
	git diff --exit-code -- Contracts/HTTP/HealthAPI/openapi.yaml Contracts/HTTP/JournalAPI/openapi.yaml Contracts/HTTP/BirthdayAPI/openapi.yaml internal/platformhttpapi/generated.go internal/healthhttpapi/generated.go internal/journalhttpapi/generated.go internal/birthdayhttpapi/generated.go internal/adminhttpapi/generated.go internal/workerhttpapi/generated.go internal/lifecyclehttpapi/generated.go

swift-client:
	swift build --package-path Contracts/HTTP

build-gateway:
	go build ./cmd/gateway

build-worker:
	go build ./cmd/worker

build-admin:
	go build ./cmd/admin
