package contracts

import (
	"os"
	"strings"
	"testing"
)

func TestCheckedInReleaseManifestParses(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../deploy/schema-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(data); err != nil {
		t.Fatalf("checked-in Swift release manifest is invalid: %v", err)
	}
}

func TestManifestRejectsSchemaNotRegisteredForOperationVersion(t *testing.T) {
	t.Parallel()

	manifest, err := ParseManifest([]byte(testManifestJSON()))
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeAndValidate(strings.NewReader(validRequestJSON("meal-decision-v10-fresh-exploration")), DefaultBodyLimit)
	if err != nil {
		t.Fatal(err)
	}
	request.ResponseSchema = []byte(`{"type":"object","properties":{"answer":{"type":"string"}},"additionalProperties":false}`)
	if err := manifest.Validate(request); err == nil {
		t.Fatal("arbitrary client schema must not turn an operation into a generic proxy")
	}
}

func TestManifestAcceptsRegisteredCanonicalSchemaIndependentOfKeyOrder(t *testing.T) {
	t.Parallel()

	manifest, err := ParseManifest([]byte(testManifestJSON()))
	if err != nil {
		t.Fatal(err)
	}
	request, _ := DecodeAndValidate(strings.NewReader(validRequestJSON("meal-decision-v10-fresh-exploration")), DefaultBodyLimit)
	request.ResponseSchema = []byte(`{"additionalProperties":false,"type":"object"}`)
	if err := manifest.Validate(request); err != nil {
		t.Fatalf("registered schema rejected: %v", err)
	}
}

func testManifestJSON() string {
	var entries []string
	for _, operation := range OperationValues() {
		policy, _ := PolicyFor(operation)
		for _, version := range []string{policy.Contract.Current, policy.Contract.Previous} {
			allowSearch := operation == OperationMealDecision
			schemaPolicy := "exact"
			schemaDigests := `["cd1a463c46d6264134447db17a8c3c7abe5b9a2488c6d759fea66da1f96b133e"]`
			if operation == OperationMealPhotoCapture || operation == OperationMealTextCapture {
				schemaPolicy = "structured"
				schemaDigests = `[]`
			}
			entries = append(entries, `{"operation":"`+string(operation)+`","contractVersion":"ai-request-v1","promptVersion":"`+version+`","schemaPolicy":"`+schemaPolicy+`","schemaSHA256":`+schemaDigests+`,"maxTemperature":1,"allowedReasoningEfforts":["","minimal","low","medium","high"],"allowStream":true,"allowWebSearch":`+boolJSON(allowSearch)+`}`)
		}
	}
	return `{"entries":[` + strings.Join(entries, ",") + `]}`
}

func boolJSON(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
