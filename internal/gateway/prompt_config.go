package gateway

import (
	"context"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"slices"
	"strings"
)

func (s *Server) freezeHealthPrompt(request *contracts.Request) *apiFailure {
	if s.promptConfig == nil || request.SystemPrompt != nil {
		return nil
	}
	r, err := s.promptConfig.Current("health:" + string(request.Operation))
	if err != nil {
		return newAPIFailure(503, "prompt_config_unavailable", "AI configuration unavailable", request.RequestID)
	}
	request.SystemPrompt = &contracts.SystemPrompt{Version: r.ID, Text: r.Policy.SystemPrompt}
	return nil
}

func (s *Server) GetJournalWritingStyles(ctx context.Context, request journalhttpapi.GetJournalWritingStylesRequestObject) (journalhttpapi.GetJournalWritingStylesResponseObject, error) {
	r, err := s.promptConfig.Current("journal")
	if err != nil {
		return journalhttpapi.GetJournalWritingStylesdefaultJSONResponse{StatusCode: 503, Body: journalErrorResponse(newAPIFailure(503, "catalog_unavailable", "Writing styles are temporarily unavailable", ""))}, nil
	}
	etag := `"` + r.ID + `"`
	cacheControl := "public, max-age=60, must-revalidate"
	if request.Params.IfNoneMatch != nil && etagMatches(*request.Params.IfNoneMatch, etag) {
		return journalhttpapi.GetJournalWritingStyles304Response{Headers: journalhttpapi.GetJournalWritingStyles304ResponseHeaders{ETag: etag, CacheControl: cacheControl}}, nil
	}
	catalog := journalhttpapi.WritingStyleCatalog{Version: r.ID, DefaultStyle: r.Policy.Journal.DefaultStyle, Styles: []journalhttpapi.WritingStyleMetadata{}}
	for _, style := range r.Policy.Journal.Styles {
		catalog.Styles = append(catalog.Styles, journalhttpapi.WritingStyleMetadata{Id: style.ID, Name: style.Name, Description: style.Description, Example: style.Example, Order: style.Order, Enabled: style.Enabled})
	}
	slices.SortStableFunc(catalog.Styles, func(a, b journalhttpapi.WritingStyleMetadata) int {
		if a.Order != b.Order {
			return a.Order - b.Order
		}
		return strings.Compare(a.Id, b.Id)
	})
	return journalhttpapi.GetJournalWritingStyles200JSONResponse{Body: catalog, Headers: journalhttpapi.GetJournalWritingStyles200ResponseHeaders{ETag: etag, CacheControl: cacheControl}}, nil
}
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		value := strings.TrimSpace(candidate)
		if value == "*" || strings.TrimPrefix(value, "W/") == etag {
			return true
		}
	}
	return false
}
