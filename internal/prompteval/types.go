// Package prompteval runs explicit administrator samples independently of user data.
package prompteval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	journal "github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"io"
	"math"
	"strings"
	"time"
)

const Retention = 7 * 24 * time.Hour
const RubricVersion = "journal-fidelity-2026-09-08"

var ErrInvalid = errors.New("invalid evaluation")
var ErrConflict = errors.New("evaluation command conflict")
var ErrNotFound = errors.New("evaluation not found")

type Sample struct {
	ID       string                   `json:"id"`
	Name     string                   `json:"name"`
	Kind     string                   `json:"kind"`
	Organize *journal.OrganizeRequest `json:"organize,omitempty"`
	Voice    *voice.Snapshot          `json:"voice,omitempty"`
	Audio    []byte                   `json:"audio,omitempty"`
	Expected []Check                  `json:"expected"`
}
type Check struct {
	Kind    string `json:"kind"`
	BlockID string `json:"blockID"`
	Text    string `json:"text"`
}

func (s Sample) Validate() error {
	if uuid.Validate(s.ID) != nil || strings.TrimSpace(s.Name) == "" || len(s.Name) > 200 || len(s.Expected) > 30 {
		return ErrInvalid
	}
	switch s.Kind {
	case "organize_lite", "organize_pro":
		if s.Organize == nil || s.Organize.Validate() != nil || s.Voice != nil || len(s.Audio) > 0 {
			return ErrInvalid
		}
	case "voice":
		if s.Voice == nil || s.Voice.Validate() != nil || s.Organize != nil || len(s.Audio) > voice.MaxSegmentBytes || len(s.Audio)%2 != 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	for _, c := range s.Expected {
		if c.Kind != "contains" && c.Kind != "excludes" && c.Kind != "unchanged" {
			return ErrInvalid
		}
		if len(c.Text) > 16000 || (c.Kind != "unchanged" && strings.TrimSpace(c.Text) == "") {
			return ErrInvalid
		}

		if c.BlockID != "" || c.Kind == "unchanged" {
			found := false
			if s.Voice != nil {
				for _, b := range s.Voice.Blocks {
					if b.ID == c.BlockID {
						found = true
					}
				}
			}
			if !found {
				return ErrInvalid
			}
		}
	}
	return nil
}

type Candidate struct {
	SourceRevision string                `json:"sourceRevision,omitempty"`
	Revision       promptconfig.Revision `json:"revision"`
	Label          string                `json:"label"`
}
type Plan struct {
	Samples       []Sample                `json:"samples"`
	Candidates    []Candidate             `json:"candidates"`
	Judge         promptconfig.Parameters `json:"judge"`
	Rubric        string                  `json:"rubric"`
	ReservedNanos int64                   `json:"reservedNanos"`
	MaximumCalls  int                     `json:"maximumCalls"`
	Requests      []json.RawMessage       `json:"requests"`
}
type Output struct {
	SpeechCostNanos    int64           `json:"speechCostNanos"`
	SpeechMilliseconds int             `json:"speechMilliseconds"`
	Candidate          int             `json:"candidate"`
	ConfigVersion      string          `json:"configVersion"`
	Model              string          `json:"model"`
	InputTokens        int             `json:"inputTokens"`
	OutputTokens       int             `json:"outputTokens"`
	CostNanos          int64           `json:"costNanos"`
	Milliseconds       int64           `json:"milliseconds"`
	Request            json.RawMessage `json:"request"`
	Structured         json.RawMessage `json:"structured,omitempty"`
	Text               string          `json:"text"`
	Checks             []CheckResult   `json:"checks"`
	Error              string          `json:"error,omitempty"`
}
type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}
type Criterion struct {
	Score    int    `json:"score"`
	Evidence string `json:"evidence"`
}
type Score struct {
	Candidate    int       `json:"candidate"`
	Fidelity     Criterion `json:"fidelity"`
	Preservation Criterion `json:"preservation"`
	Intent       Criterion `json:"intent"`
	Style        Criterion `json:"style"`
}
type Judgment struct {
	AnonymousOrder []int           `json:"anonymousOrder"`
	Request        json.RawMessage `json:"request,omitempty"`
	Model          string          `json:"model"`
	Rubric         string          `json:"rubric"`
	Status         string          `json:"status"`
	Scores         []Score         `json:"scores"`
	Error          string          `json:"error,omitempty"`
	InputTokens    int             `json:"inputTokens"`
	OutputTokens   int             `json:"outputTokens"`
	CostNanos      int64           `json:"costNanos"`
}
type ItemResult struct {
	Outputs  []Output `json:"outputs"`
	Judgment Judgment `json:"judgment"`
	Error    string   `json:"error,omitempty"`
}
type Item struct {
	Index  int         `json:"index"`
	Status string      `json:"status"`
	Result *ItemResult `json:"result,omitempty"`
}
type Run struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Plan      Plan      `json:"plan"`
	Items     []Item    `json:"items"`
}

func Prepare(samples []Sample, candidates []Candidate, judge promptconfig.Parameters, speechPrice costcontrol.DurationPrice) (Plan, error) {
	p := Plan{Samples: samples, Candidates: candidates, Judge: judge, Rubric: RubricVersion, Requests: []json.RawMessage{}}
	if len(samples) < 1 || len(samples) > 20 || len(candidates) < 1 || len(candidates) > 2 || judge.Validate() != nil || judge.Price == nil {
		return p, ErrInvalid
	}
	seen := map[string]bool{}
	for _, s := range samples {
		if s.Validate() != nil || seen[s.ID] {
			return p, ErrInvalid
		}
		seen[s.ID] = true
		judgeInputBytes := 16000
		for _, candidate := range candidates {
			r := candidate.Revision
			if r.Scope != "journal" || r.Policy.Validate("journal") != nil {
				return p, ErrInvalid
			}
			ctx := promptconfig.WithRevision(context.Background(), r)
			var raw json.RawMessage
			var parameters promptconfig.Parameters
			if s.Kind == "voice" {
				v, err := voice.PrepareRewrite(ctx, *s.Voice, 1, "")
				if err != nil {
					return p, err
				}
				raw, parameters = v.Body, v.Parameters
			} else {
				v, _, err := provider.PrepareOrganize(ctx, *s.Organize, s.Kind == "organize_pro", provider.Config{})
				if err != nil {
					return p, err
				}
				raw, parameters = v.Body, v.Parameters
			}
			if parameters.Price == nil {
				return p, ErrInvalid
			}
			extra := 0
			if len(s.Audio) > 0 {
				extra = 8 * voice.MaxContextCharacters
			}
			cost, err := parameters.Price.Cost(len(raw)+1024+extra, parameters.MaxOutputTokens)
			if err != nil {
				return p, err
			}
			if cost < 0 || cost > math.MaxInt64-p.ReservedNanos {
				return p, ErrInvalid
			}
			p.ReservedNanos += cost
			p.MaximumCalls++

			// The judge receives the source nested inside its own JSON input. Validated
			// output sizes use the production document limits, including JSON escaping.
			outputCharacters := 10000
			styleBytes := 0
			if s.Kind == "voice" {
				outputCharacters = voice.MaxContextCharacters + 4096
				style, _ := r.Policy.Journal.Style(string(s.Voice.WritingStyle))
				styleBytes = len(style.Prompt)
			}
			judgeInputBytes += 2*len(raw) + 8*outputCharacters + 8*styleBytes + 2*extra
			p.Requests = append(p.Requests, raw)
		}
		if len(s.Audio) > 0 {
			cost, err := speechPrice.Cost(voice.MaxSegmentBytes / 32)
			if err != nil {
				return p, err
			}
			if cost < 0 || cost > (math.MaxInt64-p.ReservedNanos)/int64(len(candidates)) {
				return p, ErrInvalid
			}
			p.ReservedNanos += cost * int64(len(candidates))
			p.MaximumCalls += len(candidates)
		}
		// Only production-validated outputs reach the judge.
		cost, err := judge.Price.Cost(judgeInputBytes, judge.MaxOutputTokens)
		if err != nil {
			return p, err
		}
		if cost < 0 || cost > math.MaxInt64-p.ReservedNanos {
			return p, ErrInvalid
		}
		p.ReservedNanos += cost
		p.MaximumCalls++
	}
	return p, nil
}

func (c *Criterion) UnmarshalJSON(raw []byte) error {
	var v struct {
		Score    *int    `json:"score"`
		Evidence *string `json:"evidence"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || v.Score == nil || v.Evidence == nil {
		return ErrInvalid
	}
	c.Score = *v.Score
	c.Evidence = *v.Evidence
	return nil
}
