package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminportal"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"github.com/tellyouwhat/backend/internal/prompteval"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"os"
	"strings"
	"time"
)

// synthetic-evaluation exposes only the built-in, non-user acceptance fixtures.
func syntheticEvaluation(ctx context.Context, db *sql.DB, args []string) error {
	if len(args) == 0 {
		return errors.New("use synthetic-evaluation preview, start <preview digest> <idempotency UUID>, or status <run UUID>")
	}
	cipher, err := mysqlstore.NewPayloadCipher(os.Getenv("PAYLOAD_ENCRYPTION_KEY"))
	if err != nil {
		return err
	}
	cost, err := config.LoadCostDefaults()
	if err != nil {
		return err
	}
	store := prompteval.Store{DB: db, Cipher: cipher, Limits: cost.Limits}
	if args[0] == "status" && len(args) == 2 {
		run, err := store.Get(ctx, args[1], time.Now())
		if err != nil {
			return err
		}
		rows := []map[string]any{}
		for _, i := range run.Items {
			item, err := store.Result(ctx, run.ID, i.Index, time.Now())
			if err != nil {
				return err
			}
			row := map[string]any{"sample": run.Plan.Samples[i.Index].Name, "status": item.Status}
			if item.Result != nil {
				row["judgeStatus"] = item.Result.Judgment.Status
				row["judgeModel"] = item.Result.Judgment.Model
				row["judgeError"] = item.Result.Judgment.Error
				row["judgeDiagnostic"] = item.Result.Judgment.DiagnosticOutput
				row["scores"] = item.Result.Judgment.Scores
				out := []map[string]any{}
				for _, o := range item.Result.Outputs {
					out = append(out, map[string]any{"model": o.Model, "error": o.Error, "tokens": []int{o.InputTokens, o.OutputTokens}, "checks": o.Checks, "diagnostic": o.DiagnosticOutput})
				}
				row["outputs"] = out
			}
			rows = append(rows, row)
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": run.ID, "status": run.Status, "items": rows})
	}
	if args[0] != "preview" && args[0] != "start" {
		return errors.New("unknown synthetic evaluation operation")
	}
	if args[0] == "start" && ((len(args) != 3 && len(args) != 4) || uuid.Validate(args[2]) != nil) {
		return errors.New("start requires preview digest and idempotency UUID")
	}

	sampleSelection := ""
	if args[0] == "preview" && len(args) == 2 {
		sampleSelection = args[1]
	}
	if args[0] == "start" && len(args) == 4 {
		sampleSelection = args[3]
	}
	samples := prompteval.Builtins()
	if sampleSelection != "" {
		selected := []prompteval.Sample{}
		for _, id := range strings.Split(sampleSelection, ",") {
			found := false
			for _, sample := range samples {
				if sample.ID == id {
					selected = append(selected, sample)
					found = true
					break
				}
			}
			if !found {
				return errors.New("unknown synthetic sample ID")
			}
		}
		samples = selected
	}
	var actor string
	if err = db.QueryRowContext(ctx, `SELECT id FROM admin_users WHERE role='admin' AND status='active' ORDER BY created_at LIMIT 1`).Scan(&actor); err != nil {
		return err
	}
	input := map[string]string{"source": "operator_synthetic_acceptance", "preview": ""}
	if sampleSelection != "" {
		input["sampleSelection"] = sampleSelection
	}
	m := aiconfig.Mutation{Actor: actor, RequestID: uuid.NewString()}
	if args[0] == "start" {
		m.Key = args[2]
		input["preview"] = args[1]
		if old, err := store.Replay(ctx, m, input, time.Now()); err != nil {
			return err
		} else if old != nil {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": old.ID, "status": old.Status})
		}
	}
	revision, err := (promptconfig.Store{DB: db}).Current(ctx, "journal")
	if err != nil {
		return err
	}
	inventory, err := arkcontrol.NewFromFile(os.Getenv("ARK_MANAGEMENT_CREDENTIAL_FILE"))
	if err != nil {
		return err
	}
	if err = adminportal.ResolvePromptModels(ctx, &revision.Policy, inventory); err != nil {
		return err
	}
	judge := revision.Policy.Journal.Organize.Pro
	judge.MaxOutputTokens = min(judge.MaxOutputTokens, 4096)
	plan, err := prompteval.Prepare(samples, []prompteval.Candidate{{Revision: revision, Label: "当前发布版"}}, judge, cost.JournalSpeech)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(plan)
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	if args[0] == "preview" {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"digest": digest, "samples": len(plan.Samples), "maximumCalls": plan.MaximumCalls, "reservedCNY": float64(plan.ReservedNanos) / 1e9, "models": []string{plan.Candidates[0].Revision.Policy.Journal.Organize.Lite.Model, plan.Candidates[0].Revision.Policy.Journal.Organize.Pro.Model, plan.Candidates[0].Revision.Policy.Journal.Voice.Parameters.Model}})
	}
	if digest != args[1] {
		return errors.New("preview changed; preview again before reserving budget")
	}
	run, err := store.StartInput(ctx, plan, m, input, time.Now())
	if err != nil {
		return fmt.Errorf("reserve synthetic evaluation: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": run.ID, "status": run.Status})
}
