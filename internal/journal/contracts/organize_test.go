package contracts

import (
	"strings"
	"testing"
)

func validRequest() OrganizeRequest {
	return OrganizeRequest{
		RequestID:       "19be2f9e-bd92-4699-b561-e3816092114c",
		ContractVersion: ContractVersion,
		ContentHash:     strings.Repeat("a", 64),
		Title:           "一天",
	}
}

func TestRequestRejectsMalformedIdentifiersAndUnboundedContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*OrganizeRequest)
	}{
		{name: "request id", mutate: func(r *OrganizeRequest) { r.RequestID = "not-a-uuid" }},
		{name: "content hash", mutate: func(r *OrganizeRequest) { r.ContentHash = "abc" }},
		{name: "title", mutate: func(r *OrganizeRequest) { r.Title = strings.Repeat("标", MaxTitleRunes+1) }},
		{name: "empty tag", mutate: func(r *OrganizeRequest) { r.ExistingTags = []string{" "} }},
		{name: "duplicate tag", mutate: func(r *OrganizeRequest) { r.ExistingTags = []string{"旅行", "旅行"} }},
		{name: "uncanonical tag", mutate: func(r *OrganizeRequest) { r.RejectedTagNames = []string{" 工作 "} }},
		{name: "empty book name", mutate: func(r *OrganizeRequest) {
			r.Books = []BookContext{{ID: "8d43cd74-5652-4412-b097-303f563e673a"}}
		}},
		{name: "book id", mutate: func(r *OrganizeRequest) { r.Books = []BookContext{{ID: "book-1", Name: "生活"}} }},
		{name: "book description", mutate: func(r *OrganizeRequest) {
			r.Books = []BookContext{{ID: "8d43cd74-5652-4412-b097-303f563e673a", Name: "生活", Description: strings.Repeat("长", MaxBookDescriptionRunes+1)}}
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := validRequest()
			test.mutate(&request)
			if request.Validate() == nil {
				t.Fatal("expected invalid request")
			}
		})
	}
}

func TestContractRejectsUnknownBook(t *testing.T) {
	r := validResponse()
	r.ExistingBookRecommendations = []ExistingBookRecommendation{{BookID: "b2", Reason: "相关"}}
	if r.Validate(map[string]bool{"b1": true}) == nil {
		t.Fatal("expected invalid book")
	}
}

func TestContractRejectsDuplicateOrOversizedSuggestions(t *testing.T) {
	t.Parallel()
	tests := []OrganizeResponse{
		withResponse(func(r *OrganizeResponse) { r.Tags = []Tag{{Name: "", Type: "topic"}} }),
		withResponse(func(r *OrganizeResponse) {
			r.ExistingBookRecommendations = []ExistingBookRecommendation{{BookID: "b1", Reason: "相关"}, {BookID: "b1", Reason: "仍相关"}}
		}),
		withResponse(func(r *OrganizeResponse) {
			r.ExistingBookRecommendations = []ExistingBookRecommendation{{BookID: "b1", Reason: strings.Repeat("长", MaxReasonRunes+1)}}
		}),
		withResponse(func(r *OrganizeResponse) {
			r.NewBookSuggestions = []NewBookSuggestion{{Name: "旅行", Reason: "相关"}, {Name: "旅行", Reason: "仍相关"}}
		}),
		withResponse(func(r *OrganizeResponse) { r.NewBookSuggestions = []NewBookSuggestion{{Name: "", Reason: "相关"}} }),
		withResponse(func(r *OrganizeResponse) {
			r.NewBookSuggestions = []NewBookSuggestion{{Name: "旅行", Reason: "相关", RelatedTags: make([]string, MaxRelatedTags+1)}}
		}),
	}
	for _, response := range tests {
		if response.Validate(map[string]bool{"b1": true}) == nil {
			t.Fatalf("expected invalid response: %+v", response)
		}
	}
}
func TestSchemaIsStrict(t *testing.T) {
	s := ResponseSchema()
	if s["additionalProperties"] != false {
		t.Fatal("root schema must be strict")
	}
	properties := s["properties"].(map[string]any)
	recommendations := properties["existingBookRecommendations"].(map[string]any)
	item := recommendations["items"].(map[string]any)
	itemProperties := item["properties"].(map[string]any)
	bookID := itemProperties["bookID"].(map[string]any)
	if bookID["maxLength"] != 16 {
		t.Fatalf("model book alias maxLength = %v, want 16", bookID["maxLength"])
	}
}

func validResponse() OrganizeResponse {
	return OrganizeResponse{
		RequestID:       "19be2f9e-bd92-4699-b561-e3816092114c",
		ContentHash:     strings.Repeat("a", 64),
		AnalysisVersion: "journal-organize-v1",
	}
}

func withResponse(mutate func(*OrganizeResponse)) OrganizeResponse {
	response := validResponse()
	mutate(&response)
	return response
}
