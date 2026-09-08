package adminportal

import (
	"context"
	"fmt"
	"github.com/tellyouwhat/backend/internal/airollout"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"strings"
)

// resolvePromptModels verifies IDs against Ark and replaces all client price metadata.
func (s *Server) resolvePromptModels(ctx context.Context, p *promptconfig.Policy) error {
	if p.Journal == nil {
		return nil
	}
	inventory, ok := s.config.AI.Inventory.(interface {
		Endpoint(context.Context, string) (arkcontrol.Endpoint, error)
		Versions(context.Context, string) ([]arkcontrol.Version, error)
		ModelActivations(context.Context, []string) ([]arkcontrol.Activation, error)
	})
	if !ok {
		return arkcontrol.ErrUnavailable
	}
	for _, params := range []*promptconfig.Parameters{&p.Journal.Organize.Lite, &p.Journal.Organize.Pro, &p.Journal.Voice.Parameters} {
		name, version := params.FoundationModel, params.ModelVersion
		if strings.HasPrefix(params.Model, "ep-") {
			ep, err := inventory.Endpoint(ctx, params.Model)
			if err != nil {
				return err
			}
			if ep.Status != "Running" || ep.RollingID != "" {
				return fmt.Errorf("%w: endpoint must be running with no active rollout", promptconfig.ErrInvalid)
			}
			name, version = ep.Model.FoundationModel.Name, ep.Model.FoundationModel.Version
		}
		if name == "" || version == "" {
			return promptconfig.ErrInvalid
		}
		versions, err := inventory.Versions(ctx, name)
		if err != nil {
			return err
		}
		found := false
		for _, v := range versions {
			if v.Version != version || v.Name != name {
				continue
			}
			if v.Status != "" && v.Status != "Published" {
				return promptconfig.ErrInvalid
			}
			if !strings.HasPrefix(params.Model, "ep-") && v.ModelID != params.Model {
				return promptconfig.ErrInvalid
			}
			for _, domain := range v.Domains {
				switch domain {
				case "T2I", "I2I", "T2V", "I2V", "Embedding", "EMBEDDING", "TTS", "ASR":
					return promptconfig.ErrInvalid
				}
			}
			found = true
			break
		}
		if !found {
			return promptconfig.ErrInvalid
		}
		activations, err := inventory.ModelActivations(ctx, []string{name})
		if err != nil {
			return err
		}
		params.Price = nil
		for _, a := range activations {
			if a.Name != name {
				continue
			}
			price, err := airollout.Price(a)
			if err != nil {
				return err
			}
			params.Price = &price
			break
		}
		if params.Price == nil {
			return promptconfig.ErrInvalid
		}
		params.FoundationModel, params.ModelVersion = name, version
	}
	return nil
}
