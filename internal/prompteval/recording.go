package prompteval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/voice"
)

// The manifest is frozen into the plan; PCM is encrypted in bounded rows owned by
// that run, so deleting a source sample cannot remove an active job's input.
type RecordingInput struct {
	Chunks []string `json:"chunks"`
	Bytes  int      `json:"bytes"`
	SHA256 string   `json:"sha256"`
}

func (s Store) RecordingInput(ctx context.Context, ids []string, actor string, now time.Time) (RecordingInput, error) {
	result := RecordingInput{Chunks: ids}
	seen := map[string]bool{}
	digest := sha256.New()
	if len(ids) < 1 || len(ids) > 120 {
		return result, ErrInvalid
	}
	for _, id := range ids {
		if seen[id] {
			return result, ErrInvalid
		}
		seen[id] = true
		part, err := s.SampleForActor(ctx, id, actor, now)
		if err != nil {
			return result, err
		}
		if !part.VoiceDebug || part.Kind != "voice_transcription" || len(part.Audio) == 0 {
			return result, ErrInvalid
		}
		digest.Write(part.Audio)
		result.Bytes += len(part.Audio)
	}
	if result.Bytes > 1800*32000 {
		return result, ErrInvalid
	}
	result.SHA256 = hex.EncodeToString(digest.Sum(nil))
	return result, nil
}
func (s Store) freezeRecording(ctx context.Context, tx *sql.Tx, run string, index int, manifest RecordingInput) error {
	digest := sha256.New()
	size := 0
	for part, id := range manifest.Chunks {
		var raw, nonce []byte
		var sample Sample
		if err := tx.QueryRowContext(ctx, `SELECT payload,nonce FROM prompt_eval_samples WHERE id=?`, id).Scan(&raw, &nonce); err != nil {
			return err
		}
		if err := s.open(id, raw, nonce, &sample); err != nil {
			return err
		}
		digest.Write(sample.Audio)
		size += len(sample.Audio)
		payload, salt, err := s.Cipher.Encrypt(sample.Audio, []byte(recordingKey(run, index, part)))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO prompt_eval_recording_inputs VALUES(?,?,?,?,?)`, run, index, part, payload, salt); err != nil {
			return err
		}
	}
	if size != manifest.Bytes || hex.EncodeToString(digest.Sum(nil)) != manifest.SHA256 {
		return ErrConflict
	}
	return nil
}
func recordingKey(run string, item, part int) string {
	return fmt.Sprintf("recording:%s:%d:%d", run, item, part)
}

type recordingReader struct {
	rows       *sql.Rows
	store      Store
	run        string
	item, part int
	buffer     *bytes.Reader
}

func (r *recordingReader) Read(p []byte) (int, error) {
	for {
		if r.buffer != nil && r.buffer.Len() > 0 {
			return r.buffer.Read(p)
		}
		if !r.rows.Next() {
			if err := r.rows.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		var index int
		var raw, nonce []byte
		if err := r.rows.Scan(&index, &raw, &nonce); err != nil {
			return 0, err
		}
		if index != r.part {
			return 0, ErrInvalid
		}
		data, err := r.store.Cipher.Decrypt(raw, nonce, []byte(recordingKey(r.run, r.item, index)))
		if err != nil {
			return 0, err
		}
		r.buffer = bytes.NewReader(data)
		r.part++
	}
}

type RecordingSpeech interface {
	SubmitPCM(context.Context, string, io.Reader, int) error
	Query(context.Context, string, int) (voice.RecordingAnalysis, error)
}

func (e *Engine) executeRecording(ctx context.Context, controller *costcontrol.Controller, claim Claim, sample Sample, index int, checkpoint func(Output) error) Output {
	started := time.Now()
	out := Output{Candidate: index, ConfigVersion: claim.Run.Plan.Candidates[index].Revision.ID, Model: "volc.seedasr.auc", Checks: []CheckResult{}}
	provider := e.RecordingSpeech
	if provider == nil {
		config := e.ASR
		config.URL = "https://openspeech.bytedance.com/api/v3/auc/bigmodel"
		config.ResourceID = "volc.seedasr.auc"
		provider = voice.RecordingASR{Config: config}
	}
	if previous, err := e.Store.Result(ctx, claim.Run.ID, claim.Index, time.Now()); err == nil && previous.Result != nil && len(previous.Result.Outputs) > 0 {
		out.RecordingTaskID = previous.Result.Outputs[0].RecordingTaskID
	}
	out.SpeechMilliseconds = (sample.Recording.Bytes + 31) / 32
	out.SpeechCostNanos, _ = e.SpeechPrice.Cost(out.SpeechMilliseconds)
	out.Request, _ = json.Marshal(map[string]any{"stage": "recording_transcription", "milliseconds": out.SpeechMilliseconds, "resourceID": out.Model, "sha256": sample.Recording.SHA256})
	finish := func(code string) Output {
		out.Error = code
		out.Milliseconds = time.Since(started).Milliseconds()
		return out
	}
	if out.RecordingTaskID == "" {
		lease, err := controller.Reserve(ctx, "journal", "journal.voice.recording", "speech", out.SpeechCostNanos)
		if err != nil {
			return finish(errorCode(err))
		}
		out.RecordingTaskID = uuid.NewString()
		if err = checkpoint(out); err != nil {
			_ = finishRecordingLease(ctx, lease, 0, true, costcontrol.Outcome{})
			return finish("cancelled")
		}
		rows, err := e.Store.DB.QueryContext(ctx, `SELECT part_index,payload,nonce FROM prompt_eval_recording_inputs WHERE run_id=? AND item_index=? ORDER BY part_index`, claim.Run.ID, claim.Index)
		if err != nil {
			_ = finishRecordingLease(ctx, lease, 0, true, costcontrol.Outcome{})
			return finish("provider_unavailable")
		}
		reader := &recordingReader{rows: rows, store: e.Store, run: claim.Run.ID, item: claim.Index}
		err = provider.SubmitPCM(ctx, out.RecordingTaskID, reader, sample.Recording.Bytes)
		rows.Close()
		// A lost submit response remains billable and is reconciled using this saved ID.
		_ = finishRecordingLease(ctx, lease, 0, false, costcontrol.Outcome{Success: err == nil})
	}
	for {
		analysis, err := provider.Query(ctx, out.RecordingTaskID, out.SpeechMilliseconds)
		if err == nil {
			delta := analysis.Milliseconds - out.SpeechMilliseconds
			if analysis.TaskID != out.RecordingTaskID || delta < -100 || delta > 100 || (voice.RecordingContext{Mode: "narrative", Analysis: analysis}).Validate(analysis.Text) != nil {
				return finish("output_contract_failed")
			}
			out.Structured, _ = json.Marshal(analysis)
			out.Text = analysis.Text
			out.Transcript = analysis.Text
			out.Checks = append(out.Checks, CheckResult{Name: "整段录音识别完成", Passed: true, Detail: "与录音导入使用同一识别服务"})
			return finish("")
		}
		if errors.Is(err, voice.ErrRecordingTaskMissing) && time.Since(started) >= 30*time.Second {
			return finish("recording_submission_unconfirmed")
		}
		if !errors.Is(err, voice.ErrRecordingPending) && !errors.Is(err, voice.ErrRecordingTaskMissing) {
			return finish(errorCode(err))
		}
		select {
		case <-ctx.Done():
			return finish("cancelled")
		case <-time.After(2 * time.Second):
		}
	}
}

func (s Store) ReconcileRecording(ctx context.Context, id string, index int, actor string, now time.Time) error {
	run, err := s.GetForActor(ctx, id, actor, now)
	if err != nil {
		return err
	}
	if run.Plan.Mode != VoiceDebugMode || index < 0 || index >= len(run.Plan.Samples) || run.Plan.Samples[index].Kind != "voice_recording" {
		return ErrInvalid
	}
	item, err := s.Result(ctx, id, index, now)
	if err != nil {
		return err
	}
	if item.Result == nil || len(item.Result.Outputs) != 1 || item.Result.Outputs[0].RecordingTaskID == "" {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE prompt_eval_items SET status='pending_debug' WHERE run_id=? AND item_index=? AND status IN ('failed','interrupted')`, id, index)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE prompt_eval_runs SET status='pending' WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func finishRecordingLease(parent context.Context, lease *costcontrol.Lease, cost int64, known bool, outcome costcontrol.Outcome) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	return lease.Finish(ctx, cost, known, outcome)
}
