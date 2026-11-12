package prompteval

import (
	"context"
	"encoding/json"
	"math"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

const VoiceDebugMode = "voice_debug"

// PrepareVoiceDebug uses the durable evaluation queue without an extra AI judge.
// Audio is bounded to the same segment size as live dictation; the browser batches
// longer recordings instead of sending one large JSON request to the admin API.
func PrepareVoiceDebug(samples []Sample, candidate Candidate, speechPrice costcontrol.DurationPrice) (Plan, error) {
	p := Plan{Mode: VoiceDebugMode, Samples: samples, Candidates: []Candidate{candidate}, Requests: []json.RawMessage{}}
	if len(samples) < 1 || len(samples) > 20 || candidate.Revision.Scope != "journal" || candidate.Revision.Policy.Validate("journal") != nil {
		return p, ErrInvalid
	}
	seen := map[string]bool{}
	ctx := promptconfig.WithRevision(context.Background(), candidate.Revision)
	for _, sample := range samples {
		if sample.Validate() != nil || !sample.VoiceDebug || seen[sample.ID] {
			return p, ErrInvalid
		}
		seen[sample.ID] = true
		var cost int64
		var err error
		var raw json.RawMessage
		switch sample.Kind {
		case "voice_recording":
			if len(samples) != 1 {
				return p, ErrInvalid
			}
			cost, err = speechPrice.Cost((sample.Recording.Bytes + 31) / 32)
			raw, _ = json.Marshal(map[string]any{"stage": "recording_transcription", "resourceID": "volc.seedasr.auc", "milliseconds": (sample.Recording.Bytes + 31) / 32, "sha256": sample.Recording.SHA256})
		case "voice_transcription":
			// BudgetedSpeech admits one full billing window before sending audio.
			cost, err = speechPrice.Cost(voice.MaxSegmentBytes / 32)
			raw = transcriptionRequest(candidate.Revision, sample)
		case "voice":
			if len(sample.Audio) > 0 || sample.Voice.Polish == nil || sample.Voice.Polish.Paused || sample.Voice.Polish.Lightweight || len(sample.Voice.Polish.Targets) == 0 {
				return p, ErrInvalid
			}
			prepared, prepareErr := voice.PrepareRewrite(ctx, sample.voiceSnapshot(), 1, "")
			if prepareErr != nil || prepared.Parameters.Price == nil {
				return p, ErrInvalid
			}
			raw = prepared.Body
			cost, err = prepared.Parameters.Price.Cost(len(raw)+1024, prepared.Parameters.MaxOutputTokens)
		default:
			return p, ErrInvalid
		}
		if err != nil || cost <= 0 || cost > math.MaxInt64-p.ReservedNanos {
			return p, ErrInvalid
		}
		p.Requests = append(p.Requests, raw)
		p.ReservedNanos += cost
		p.MaximumCalls++
	}
	return p, nil
}

func transcriptionRequest(revision promptconfig.Revision, sample Sample) json.RawMessage {
	settings := revision.Policy.Journal.Voice
	raw, _ := json.Marshal(map[string]any{"stage": "transcription", "milliseconds": (len(sample.Audio) + 31) / 32, "format": "PCM16", "sampleRate": 16000, "channels": 1, "words": sample.Voice.Words, "automaticPunctuation": settings.AutomaticPunctuation, "normalizeNumbers": settings.NormalizeNumbers})
	return raw
}

// Reconstruct the server-owned finalization context after encrypted queue decoding.
func (s Sample) voiceSnapshot() voice.Snapshot {
	snapshot := *s.Voice
	snapshot.Organization = snapshot.Polish != nil && !snapshot.Polish.Lightweight
	return snapshot
}
