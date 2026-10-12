package birthdayhttpapi

import (
	"context"
	"strings"
	"testing"
)

func TestBirthdayEmbeddedContractUsesSharedIdentityAndNamedOperation(t *testing.T) {
	document, err := GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(document.Paths.Map()) != 12 || !strings.EqualFold(document.Paths.Find("/v2/birthday/assist").Post.OperationID, "assistBirthday") {
		t.Fatalf("birthday route drifted: count=%d operation=%s", len(document.Paths.Map()), document.Paths.Find("/v2/birthday/assist").Post.OperationID)
	}
	if document.Components.SecuritySchemes["AppAttestKeyID"] == nil {
		t.Fatal("shared device identity missing")
	}
}
