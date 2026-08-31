package service

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journal/provider"
)

type Model interface {
	Organize(context.Context, contracts.OrganizeRequest, bool) (provider.Result, error)
}
type Organizer struct {
	Model                                        Model
	LiteMaxCharacters, LiteMaxBooks, LiteMaxTags int
	AnalysisVersion                              string
}

func (s Organizer) Organize(ctx context.Context, request contracts.OrganizeRequest) (provider.Result, error) {
	pro := utf8.RuneCountInString(request.Body) > s.LiteMaxCharacters || len(request.Books) > s.LiteMaxBooks || len(request.ExistingTags) > s.LiteMaxTags
	result, err := s.Model.Organize(ctx, request, pro)
	if err == nil || pro || !errors.Is(err, provider.ErrInvalidResult) {
		return result, err
	}
	return s.Model.Organize(ctx, request, true)
}
