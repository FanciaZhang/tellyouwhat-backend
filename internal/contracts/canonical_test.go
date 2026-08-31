package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestRequestBindingDigestUsesMethodPathRequestNonceTimestampAndBody(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../Contracts/Fixtures/managed-request-binding.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Method                string `json:"method"`
		Path                  string `json:"path"`
		RequestID             string `json:"requestID"`
		Nonce                 string `json:"nonce"`
		Timestamp             string `json:"timestamp"`
		BodySHA256            string `json:"bodySHA256"`
		ExpectedBindingSHA256 string `json:"expectedBindingSHA256"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}

	digest := RequestBindingDigest(RequestBinding{
		Method: fixture.Method, Path: fixture.Path, RequestID: fixture.RequestID,
		Nonce: fixture.Nonce, Timestamp: fixture.Timestamp, BodySHA256: fixture.BodySHA256,
	})
	if digest != fixture.ExpectedBindingSHA256 {
		t.Fatalf("unexpected binding digest: %s", digest)
	}
}

func TestSwiftManagedRequestFixtureHasStableCanonicalBody(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../Contracts/Fixtures/managed-request-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Request                     json.RawMessage `json:"request"`
		ExpectedCanonicalBodySHA256 string          `json:"expectedCanonicalBodySHA256"`
		UpgradeRequiredError        struct {
			Status int `json:"status"`
			Body   struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"body"`
		} `json:"upgradeRequiredError"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	digest, err := CanonicalJSONSHA256(fixture.Request)
	if err != nil {
		t.Fatal(err)
	}
	if digest != fixture.ExpectedCanonicalBodySHA256 {
		t.Fatalf("canonical request digest drifted: %s", digest)
	}
	if _, err := DecodeAndValidate(
		bytes.NewReader(fixture.Request),
		DefaultBodyLimit,
	); err != nil {
		t.Fatalf("shared Swift request no longer satisfies the Go contract: %v", err)
	}
	if fixture.UpgradeRequiredError.Status != 426 || fixture.UpgradeRequiredError.Body.Error.Code != "upgrade_required" {
		t.Fatal("shared upgrade error protocol drifted")
	}
}
