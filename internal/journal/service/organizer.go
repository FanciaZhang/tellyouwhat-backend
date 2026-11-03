package service

import (
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"unicode/utf8"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journal/provider"
)

type Model interface {
	Organize(context.Context, contracts.OrganizeRequest, bool) (provider.Result, error)
}
type Organizer struct {
	Model                                        Model
	Config                                       *promptconfig.Cache
	LiteMaxCharacters, LiteMaxBooks, LiteMaxTags int
	AnalysisVersion                              string
}

func (s Organizer) Organize(ctx context.Context, request contracts.OrganizeRequest) (provider.Result, error) {
	if s.Config != nil {
		var err error
		ctx, err = promptconfig.Freeze(ctx, s.Config)
		if err != nil {
			return provider.Result{}, err
		}
	}
	if r, ok := promptconfig.FromContext(ctx); ok {
		s.LiteMaxCharacters = r.Policy.Journal.Organize.LiteMaxCharacters
		s.LiteMaxBooks = r.Policy.Journal.Organize.LiteMaxBooks
		s.LiteMaxTags = r.Policy.Journal.Organize.LiteMaxTags
	}
	pro := utf8.RuneCountInString(request.Body) > s.LiteMaxCharacters || len(request.Books) > s.LiteMaxBooks || len(request.ExistingTags) > s.LiteMaxTags
	result, err := s.Model.Organize(ctx, request, pro)
	if err == nil || pro || !errors.Is(err, provider.ErrInvalidResult) {
		return result, err
	}
	retry, retryErr := s.Model.Organize(ctx, request, true)
	retry.InputTokens += result.InputTokens
	retry.OutputTokens += result.OutputTokens
	return retry, retryErr
}
