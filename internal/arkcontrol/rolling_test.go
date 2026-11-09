package arkcontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNativeRollingWireContractAndNoAutomaticRetry(t *testing.T) {
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("Action")
		calls[action]++
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid JSON")
		}
		switch action {
		case "CreateEndpointRolling":
			if body["EndpointId"] != "ep-test" || body["ModelReference"].(map[string]any)["FoundationModel"].(map[string]any)["ModelVersion"] != "260428" {
				t.Error("native model reference not encoded")
			}
			if body["DryRun"] == true {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"ResponseMetadata":{"Error":{"Code":"DryRunOperation","Message":"accepted"}}}`)
				return
			}
			w.WriteHeader(503)
			fmt.Fprint(w, `{"ResponseMetadata":{"Error":{"Code":"InternalError","Message":"PRIVATE DETAIL"}}}`)
		case "GetEndpointRolling":
			fmt.Fprint(w, `{"Result":{"RollingInfo":{"Id":"eprol-test","EndpointId":"ep-test","Status":"Running","RollingGray":4,"RollingStrategy":{"Step":2,"WaitDuration":1}}}}`)
		case "CancelEndpointRolling", "RollbackEndpointRolling":
			if body["Id"] != "eprol-test" {
				t.Error("wrong rolling ID")
			}
			fmt.Fprint(w, `{"Result":{"Id":"eprol-test"}}`)
		default:
			t.Error("unexpected cloud action")
		}
	}))
	defer server.Close()
	c, _ := newClient("test-ak", "test-sk", server.URL, server.Client())
	ctx := context.Background()
	target := FoundationModel{Name: "doubao-seed-2-0-mini", Version: "260428"}
	if err := c.PreviewRolling(ctx, "ep-test", target); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRolling(ctx, "ep-test", target, "command-1"); err == nil {
		t.Fatal("ambiguous failure accepted")
	}
	if calls["CreateEndpointRolling"] != 2 {
		t.Fatal("retried a non-idempotent cloud mutation")
	}
	rolling, err := c.Rolling(ctx, "eprol-test")
	if err != nil || rolling.Gray != 4 || rolling.Strategy.Step != 2 {
		t.Fatal("native state lost")
	}
	if err := c.RollbackRolling(ctx, "eprol-test"); err != nil {
		t.Fatal(err)
	}
	if err := c.CancelRolling(ctx, "eprol-test"); err != nil {
		t.Fatal(err)
	}
}
