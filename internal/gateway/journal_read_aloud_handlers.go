package gateway

import (
	"context"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
)

type ReadAloudCatalogReader interface {
	Current(context.Context) (readaloud.CatalogRevision, error)
}

func (s *Server) GetJournalReadAloudVoices(ctx context.Context, r journalhttpapi.GetJournalReadAloudVoicesRequestObject) (journalhttpapi.GetJournalReadAloudVoicesResponseObject, error) {
	deny := func() (journalhttpapi.GetJournalReadAloudVoicesResponseObject, error) {
		return journalhttpapi.GetJournalReadAloudVoicesdefaultJSONResponse{StatusCode: 503, Body: journalErrorResponse(newAPIFailure(503, "voice_catalog_unavailable", "Voice catalog is temporarily unavailable", ""))}, nil
	}
	if s.readAloudCatalog == nil {
		return deny()
	}
	current, err := s.readAloudCatalog.Current(ctx)
	if err != nil {
		return deny()
	}
	public, err := current.Public()
	if err != nil {
		return deny()
	}
	etag := `"` + public.Version + `"`
	cache := "public, max-age=14400, must-revalidate"
	if r.Params.IfNoneMatch != nil && etagMatches(*r.Params.IfNoneMatch, etag) {
		return journalhttpapi.GetJournalReadAloudVoices304Response{Headers: journalhttpapi.GetJournalReadAloudVoices304ResponseHeaders{ETag: etag, CacheControl: cache}}, nil
	}
	body := journalhttpapi.ReadAloudCatalog{Version: public.Version, DefaultVoiceID: public.DefaultVoiceID, MaxAgeSeconds: public.MaxAgeSeconds, Voices: []journalhttpapi.ReadAloudVoice{}}
	for _, v := range public.Voices {
		voice := journalhttpapi.ReadAloudVoice{Id: v.ID, Name: v.Name, Description: v.Description, Languages: v.Languages, Styles: v.Styles, Dialects: v.Dialects, SupportsPitch: v.SupportsPitch, Revision: v.Revision}
		if v.PreviewURL != "" {
			url := v.PreviewURL
			voice.PreviewURL = &url
		}
		body.Voices = append(body.Voices, voice)
	}
	return journalhttpapi.GetJournalReadAloudVoices200JSONResponse{Body: body, Headers: journalhttpapi.GetJournalReadAloudVoices200ResponseHeaders{ETag: etag, CacheControl: cache}}, nil
}
