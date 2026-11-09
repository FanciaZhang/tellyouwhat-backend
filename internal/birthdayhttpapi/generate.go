// Package birthdayhttpapi contains the generated Birthday HTTP contract.
package birthdayhttpapi

//go:generate go run ../../cmd/openapi-compose -platform ../../Contracts/HTTP/PlatformAPI/openapi.yaml -app ../../Contracts/HTTP/BirthdayAPI/app.openapi.yaml -output ../../Contracts/HTTP/BirthdayAPI/openapi.yaml
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 --config=codegen.yaml ../../Contracts/HTTP/BirthdayAPI/openapi.yaml
