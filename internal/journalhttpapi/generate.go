// Package journalhttpapi contains code generated from the canonical Journal HTTP contract.
package journalhttpapi

//go:generate go run ../../cmd/openapi-compose -platform ../../Contracts/HTTP/PlatformAPI/openapi.yaml -app ../../Contracts/HTTP/JournalAPI/app.openapi.yaml -output ../../Contracts/HTTP/JournalAPI/openapi.yaml
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 --config=codegen.yaml ../../Contracts/HTTP/JournalAPI/openapi.yaml
