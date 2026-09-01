// Package healthhttpapi contains code generated from the canonical Health HTTP contract.
package healthhttpapi

//go:generate go run ../../cmd/openapi-compose -platform ../../Contracts/HTTP/PlatformAPI/openapi.yaml -app ../../Contracts/HTTP/HealthAPI/app.openapi.yaml -output ../../Contracts/HTTP/HealthAPI/openapi.yaml
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 --config=codegen.yaml ../../Contracts/HTTP/HealthAPI/openapi.yaml
