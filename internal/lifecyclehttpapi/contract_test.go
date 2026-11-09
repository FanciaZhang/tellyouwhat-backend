package lifecyclehttpapi

import (
	"context"
	"testing"
)

func TestLifecycleContract(t *testing.T) {
	document, err := GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	if err = document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if document.Paths.Len() != 2 {
		t.Fatal("unexpected lifecycle route table")
	}
}
