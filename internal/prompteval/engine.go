package prompteval

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"strings"
	"sync"
	"time"
)

type Engine struct {
	Store       Store
	Provider    provider.Config
	ASR         voice.ASRConfig
	Price       costcontrol.TokenPrice
	SpeechPrice costcontrol.DurationPrice
}

func (e *Engine) Run(ctx context.Context) {
	var workers sync.WaitGroup
	defer workers.Wait()
	timer := time.NewTicker(2 * time.Second)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_ = e.Store.Recover(ctx, time.Now())
		for range 2 {
			claim, err := e.Store.Claim(ctx, time.Now())
			if err != nil {
				break
			}
			workers.Add(1)
			go func() { defer workers.Done(); e.Process(ctx, claim) }()
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
func (e *Engine) Process(parent context.Context, claim Claim) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Hour)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		timer := time.NewTicker(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-timer.C:
				if !e.Store.Heartbeat(ctx, claim, time.Now()) {
					cancel()
					return
				}
			}
		}
	}()
	controller, err := costcontrol.New(BatchBudget{Store: e.Store, RunID: claim.Run.ID}, e.Store.Limits, time.Now)
	result := ItemResult{Outputs: []Output{}, Judgment: Judgment{Status: "incomplete", Rubric: RubricVersion, Scores: []Score{}}}
	if err != nil {
		result.Error = "budget_unavailable"
		e.finish(claim, result)
		return
	}
	sample := claim.Run.Plan.Samples[claim.Index]
	for i, candidate := range claim.Run.Plan.Candidates {
		if ctx.Err() != nil {
			result.Error = "cancelled_or_interrupted"
			break
		}
		frozen := promptconfig.WithRevision(ctx, candidate.Revision)
		output := e.execute(frozen, controller, sample, candidate, i)
		result.Outputs = append(result.Outputs, output)
		if err = e.Store.SaveResult(ctx, claim, result, false); err != nil {
			cancel()
			return
		}
	}
	if ctx.Err() == nil && len(result.Outputs) == len(claim.Run.Plan.Candidates) {
		complete := true
		for _, output := range result.Outputs {
			if output.Error != "" {
				complete = false
			}
		}
		if complete {
			result.Judgment = e.judge(ctx, controller, claim.Run.Plan, sample, result.Outputs)
		} else {
			result.Judgment.Error = "candidate_incomplete"
		}
	}
	e.finish(claim, result)
}
func (e *Engine) finish(c Claim, result ItemResult) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = e.Store.SaveResult(ctx, c, result, true)
	_ = e.Store.Recover(ctx, time.Now())
}
func (e *Engine) execute(ctx context.Context, controller *costcontrol.Controller, sample Sample, candidate Candidate, index int) Output {
	started := time.Now()
	out := Output{Candidate: index, ConfigVersion: candidate.Revision.ID, Checks: []CheckResult{}}
	var parameters promptconfig.Parameters
	var callErr error
	var blocks []voice.Block
	if sample.Kind == "voice" {
		snapshot := *sample.Voice
		if len(sample.Audio) > 0 {
			speech := voice.NewBudgetedSpeech(voice.ASR{Config: e.ASR}, controller, "journal", e.SpeechPrice)
			transcript, err := transcribe(ctx, speech, sample.Audio, snapshot.Words)
			if err != nil {
				out.Error = errorCode(err)
				return out
			}
			snapshot.Transcript += transcript
			out.SpeechMilliseconds = (len(sample.Audio) + 31) / 32
			out.SpeechCostNanos, _ = e.SpeechPrice.Cost(out.SpeechMilliseconds)
		}
		prepared, err := voice.PrepareRewrite(ctx, snapshot, 1, "")
		if err != nil {
			out.Error = errorCode(err)
			return out
		}
		out.Request, parameters = prepared.Body, prepared.Parameters
		model := voice.NewBudgetedRewriter(voice.ArkRewriter{BaseURL: e.Provider.BaseURL, APIKey: e.Provider.APIKey}, controller, "journal", e.Price)
		value, err := model.Rewrite(ctx, snapshot, 1)
		callErr = err
		out.Model, out.InputTokens, out.OutputTokens = value.Model, value.InputTokens, value.OutputTokens
		if err == nil {
			blocks, callErr = voice.ApplyRevision(snapshot, value.Revision)
			out.Structured, _ = json.Marshal(value.Revision)
			for _, b := range blocks {
				out.Text += b.Text + "\n"
			}
		}
	} else {
		prepared, _, err := provider.PrepareOrganize(ctx, *sample.Organize, sample.Kind == "organize_pro", e.Provider)
		if err != nil {
			out.Error = errorCode(err)
			return out
		}
		out.Request, parameters = prepared.Body, prepared.Parameters
		model := provider.NewBudgetedClient(provider.New(e.Provider, nil), controller, "journal", e.Price)
		value, err := model.Organize(ctx, *sample.Organize, sample.Kind == "organize_pro")
		callErr = err
		out.Model, out.InputTokens, out.OutputTokens = value.Model, value.InputTokens, value.OutputTokens
		if err == nil {
			out.Structured, _ = json.Marshal(value.Value)
			out.Text = string(out.Structured)
		}
	}
	out.Milliseconds = time.Since(started).Milliseconds()
	if parameters.Price != nil {
		out.CostNanos, _ = parameters.Price.Cost(out.InputTokens, out.OutputTokens)
	}
	out.Checks = append(out.Checks, CheckResult{Name: "生产协议与正文约束", Passed: callErr == nil, Detail: "使用生产请求组装、响应解析与段落应用校验"}, CheckResult{Name: "实际模型可追溯", Passed: out.Model != "", Detail: out.Model})
	if callErr != nil {
		out.Error = errorCode(callErr)
		return out
	}
	for _, check := range sample.Expected {
		text := out.Text
		if check.BlockID != "" {
			text = ""
			for _, b := range blocks {
				if b.ID == check.BlockID {
					text = b.Text
					break
				}
			}
		}
		passed := false
		switch check.Kind {
		case "contains":
			passed = strings.Contains(text, check.Text)
		case "excludes":
			passed = !strings.Contains(text, check.Text)
		case "unchanged":
			if sample.Voice != nil {
				for _, b := range sample.Voice.Blocks {
					if b.ID == check.BlockID {
						passed = text == b.Text
						break
					}
				}
			}
		}
		out.Checks = append(out.Checks, CheckResult{Name: check.Kind, Passed: passed, Detail: check.Text})
	}
	return out
}
func errorCode(err error) string {
	switch {
	case errors.Is(err, costcontrol.ErrBudgetExceeded):
		return "budget_unavailable"
	case errors.Is(err, costcontrol.ErrConcurrencyExceeded):
		return "concurrency_unavailable"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, voice.ErrInvalid), errors.Is(err, voice.ErrConflict), errors.Is(err, provider.ErrInvalidResult):
		return "output_contract_failed"
	default:
		return "provider_unavailable"
	}
}
func transcribe(ctx context.Context, speech voice.Speech, audio []byte, words []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	connection, err := speech.Open(ctx, words)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			connection.Close()
		case <-done:
		}
	}()
	for offset := 0; offset < len(audio); {
		end := min(len(audio), offset+6400)
		if err = connection.Send(audio[offset:end], end == len(audio)); err != nil {
			return "", err
		}
		offset = end
	}
	for {
		value, err := connection.Receive()
		if err != nil {
			return "", err
		}
		if value.Final {
			return value.Text, nil
		}
	}
}
