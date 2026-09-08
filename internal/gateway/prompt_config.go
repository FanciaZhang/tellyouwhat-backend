package gateway

import "github.com/tellyouwhat/backend/internal/contracts"

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
