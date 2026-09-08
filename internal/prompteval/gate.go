package prompteval

import (
	"context"
	"database/sql"
	"errors"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"time"
)

// PublicationBlockers checks the latest run containing this exact configuration.
// An advisory judge failure is displayed separately and does not become a pass.
func (s Store) PublicationBlockers(ctx context.Context, r promptconfig.Revision, now time.Time) ([]string, error) {
	if r.Scope != "journal" {
		return []string{}, nil
	}
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM prompt_eval_runs WHERE JSON_CONTAINS(candidate_revisions,JSON_QUOTE(?)) AND expires_at>? ORDER BY created_at DESC,id DESC LIMIT 1`, r.ID, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{"请先评测这个草稿，覆盖智能整理 Lite、Pro 与语音整理"}, nil
	}
	if err != nil {
		return nil, err
	}
	run, err := s.Get(ctx, id, now)
	if err != nil {
		return nil, err
	}
	if run.Status != "completed" && run.Status != "completed_with_failures" {
		return []string{"评测尚未完成，或已经取消"}, nil
	}
	candidate := -1
	for i, c := range run.Plan.Candidates {
		if c.Revision.ID == r.ID {
			candidate = i
			break
		}
	}
	if candidate < 0 {
		return []string{"未找到对应配置的评测结果"}, nil
	}
	coverage := map[string]bool{}
	for _, entry := range run.Items {
		item, err := s.Result(ctx, id, entry.Index, now)
		if err != nil {
			return nil, err
		}
		if item.Status == "interrupted" || item.Status == "cancelled" || item.Result == nil {
			return []string{"存在中断或缺失的样例结果"}, nil
		}
		found := false
		for _, output := range item.Result.Outputs {
			if output.Candidate != candidate {
				continue
			}
			found = true
			if output.Error != "" || len(output.Checks) == 0 {
				return []string{"候选输出未通过协议检查，请修正后重新评测"}, nil
			}
			for _, check := range output.Checks {
				if !check.Passed {
					return []string{"候选输出未通过协议、引用或正文保护检查"}, nil
				}
			}
		}
		if !found {
			return []string{"缺少候选版本输出"}, nil
		}
		coverage[run.Plan.Samples[entry.Index].Kind] = true
	}
	for _, kind := range []string{"organize_lite", "organize_pro", "voice"} {
		if !coverage[kind] {
			return []string{"评测需要覆盖智能整理 Lite、Pro 与语音整理"}, nil
		}
	}
	return []string{}, nil
}
