package contracts

import "testing"

func TestContractRejectsUnknownBook(t *testing.T) {
	r := OrganizeResponse{ExistingBookRecommendations: []ExistingBookRecommendation{{BookID: "b2", Reason: "相关"}}}
	if r.Validate(map[string]bool{"b1": true}) == nil {
		t.Fatal("expected invalid book")
	}
}
func TestSchemaIsStrict(t *testing.T) {
	s := ResponseSchema()
	if s["additionalProperties"] != false {
		t.Fatal("root schema must be strict")
	}
}
