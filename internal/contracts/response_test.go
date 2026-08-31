package contracts

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateResponseEnforcesClientArtifactSchemaLocally(t *testing.T) {
	t.Parallel()

	request := Request{ResponseSchema: json.RawMessage(`{"type":"object","properties":{"choice":{"type":"string"}},"required":["choice"],"additionalProperties":false}`)}
	if err := ValidateResponse(request, `{"choice":"soup"}`); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	if err := ValidateResponse(request, `{"answer":42}`); !errors.Is(err, ErrContractViolation) {
		t.Fatalf("schema violation not rejected: %v", err)
	}
}

func TestValidateResponseRejectsRemoteSchemaReference(t *testing.T) {
	t.Parallel()

	request := Request{ResponseSchema: json.RawMessage(`{"$ref":"https://attacker.invalid/schema.json","type":"object","additionalProperties":false}`)}
	if err := ValidateResponse(request, `{}`); !errors.Is(err, ErrContractViolation) {
		t.Fatalf("remote schema reference not rejected: %v", err)
	}
}

func TestRepairUnquotedStringValuesFixesMissingOpeningQuotes(t *testing.T) {
	t.Parallel()

	content := `{"components":[{"food_items":[{"name":"炒南瓜","estimated_amount":1份","confidence":0.9}]}]}`
	repaired, ok := RepairUnquotedStringValues(content)
	if !ok {
		t.Fatalf("expected repair to apply")
	}
	if !json.Valid([]byte(repaired)) {
		t.Fatalf("repaired content is still invalid JSON: %s", repaired)
	}
	if !strings.Contains(repaired, `"estimated_amount":"1份"`) {
		t.Fatalf("unexpected repaired content: %s", repaired)
	}
}

func TestRepairUnquotedStringValuesLeavesValidJSONUntouched(t *testing.T) {
	t.Parallel()

	content := `{"estimated_amount":"1份"}`
	repaired, ok := RepairUnquotedStringValues(content)
	if ok || repaired != content {
		t.Fatalf("valid JSON must not be rewritten: %q %v", repaired, ok)
	}
}

func TestRepairUnquotedStringValuesDoesNotRewriteLiterals(t *testing.T) {
	t.Parallel()

	content := `{"count":1,"ok":true,"missing":null}`
	repaired, ok := RepairUnquotedStringValues(content)
	if ok || repaired != content {
		t.Fatalf("literal values must not be rewritten: %q %v", repaired, ok)
	}
}

func TestCanonicalizeFoodGroupSynonymsFixesLegumes(t *testing.T) {
	t.Parallel()

	content := `{"diet_evidence":{"food_group_contributions":[{"group":"legumes","food_item_ids":["f1"],"portion_share":1,"confidence":0.9}]}}`
	canonicalized, ok := CanonicalizeFoodGroupSynonyms(content)
	if !ok {
		t.Fatalf("expected synonym canonicalization to apply")
	}
	if !strings.Contains(canonicalized, `"group":"soy"`) {
		t.Fatalf("unexpected canonicalized content: %s", canonicalized)
	}
	if !json.Valid([]byte(canonicalized)) {
		t.Fatalf("canonicalized content is invalid JSON: %s", canonicalized)
	}
}

func TestCanonicalizeFoodGroupSynonymsLeavesCanonicalAndUnknownUntouched(t *testing.T) {
	t.Parallel()

	content := `{"diet_evidence":{"food_group_contributions":[{"group":"soy","food_item_ids":["f1"],"portion_share":1,"confidence":0.9},{"group":"custom_group","food_item_ids":["f2"],"portion_share":1,"confidence":0.9}]}}`
	canonicalized, ok := CanonicalizeFoodGroupSynonyms(content)
	if ok || canonicalized != content {
		t.Fatalf("canonical and unknown values must not be rewritten: %q %v", canonicalized, ok)
	}
}

