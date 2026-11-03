package gateway

import (
	"context"
	"encoding/json"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"strings"
	"testing"
	"time"
)

type catalogFixture struct{}

func (catalogFixture) Published(context.Context) (map[string]promptconfig.Revision, error) {
	now := time.Now()
	out := map[string]promptconfig.Revision{}
	for scope, p := range promptconfig.Defaults("private-endpoint", "private-pro", "private-voice", 90) {
		out[scope] = promptconfig.Revision{ID: "catalog-1", Scope: scope, Policy: p, PublishedAt: &now}
	}
	return out, nil
}
func TestWritingStyleCatalogMetadataAndConditionalRequest(t *testing.T) {
	ctx := context.Background()
	cache := promptconfig.NewCache(catalogFixture{})
	if err := cache.Refresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	server := Server{promptConfig: cache}
	response, err := server.GetJournalWritingStyles(ctx, journalhttpapi.GetJournalWritingStylesRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	catalog, ok := response.(journalhttpapi.GetJournalWritingStyles200JSONResponse)
	if !ok || len(catalog.Body.Styles) != 5 || catalog.Body.DefaultStyle != "natural" {
		t.Fatal(response)
	}
	raw, _ := json.Marshal(catalog.Body)
	for _, private := range []string{"prompt", "private-endpoint", "reasoningEffort", "temperature"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private configuration leaked", private)
		}
	}
	header := `W/"catalog-1", "other"`
	response, err = server.GetJournalWritingStyles(ctx, journalhttpapi.GetJournalWritingStylesRequestObject{Params: journalhttpapi.GetJournalWritingStylesParams{IfNoneMatch: &header}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := response.(journalhttpapi.GetJournalWritingStyles304Response); !ok {
		t.Fatal("ETag not honored", response)
	}
}
