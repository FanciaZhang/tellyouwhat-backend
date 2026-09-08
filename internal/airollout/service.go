package airollout

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

type Cloud interface {
	Endpoint(context.Context, string) (arkcontrol.Endpoint, error)
	Rolling(context.Context, string) (arkcontrol.Rolling, error)
	Versions(context.Context, string) ([]arkcontrol.Version, error)
	ModelActivations(context.Context, []string) ([]arkcontrol.Activation, error)
	PreviewRolling(context.Context, string, arkcontrol.FoundationModel) error
	CreateRolling(context.Context, string, arkcontrol.FoundationModel, string) (string, error)
	CancelRolling(context.Context, string) error
	RollbackRolling(context.Context, string) error
}
type Service struct {
	Store         Store
	Cloud         Cloud
	Endpoints     map[contracts.Operation]string
	Shared        map[string]bool
	WritesEnabled bool
	priceMu       sync.Mutex
	priceCache    map[string]cachedPrice
}
type cachedPrice struct {
	price costcontrol.TokenPrice
	at    time.Time
}

func (s *Service) Allowed(id string) bool {
	if id == "" || s.Shared[id] {
		return false
	}
	for _, owned := range s.Endpoints {
		if id == owned {
			return true
		}
	}
	return false
}
func endpointModel(ep arkcontrol.Endpoint) arkcontrol.FoundationModel {
	return arkcontrol.FoundationModel{Name: ep.Model.FoundationModel.Name, Version: ep.Model.FoundationModel.Version}
}
func (s *Service) modelPrice(ctx context.Context, m arkcontrol.FoundationModel) (costcontrol.TokenPrice, error) {
	if !Known(m) {
		return costcontrol.TokenPrice{}, ErrUnsupported
	}
	s.priceMu.Lock()
	defer s.priceMu.Unlock()
	if p, ok := s.priceCache[m.Name]; ok && time.Since(p.at) < time.Minute {
		return p.price, nil
	}
	activations, err := s.Cloud.ModelActivations(ctx, []string{m.Name})
	if err != nil {
		return costcontrol.TokenPrice{}, err
	}
	for _, a := range activations {
		if a.Name == m.Name {
			p, err := Price(a)
			if err != nil {
				return p, err
			}
			if s.priceCache == nil {
				s.priceCache = map[string]cachedPrice{}
			}
			s.priceCache[m.Name] = cachedPrice{p, time.Now()}
			return p, nil
		}
	}
	return costcontrol.TokenPrice{}, ErrUnsupported
}
func (s *Service) Read(ctx context.Context, id string) (Snapshot, error) {
	out := Snapshot{Catalog: CatalogVersion}
	if !s.Allowed(id) {
		return out, ErrUnsupported
	}
	ep, err := s.Cloud.Endpoint(ctx, id)
	if err != nil {
		return out, err
	}
	out.Endpoint = ep
	if ep.ID != id || ep.Status != "Running" {
		return out, ErrUnsupported
	}
	out.Price, err = s.modelPrice(ctx, endpointModel(ep))
	if err != nil {
		return out, err
	}
	if ep.RollingID != "" {
		r, err := s.Cloud.Rolling(ctx, ep.RollingID)
		if err != nil {
			return out, err
		}
		if r.EndpointID != id || r.ID != ep.RollingID || r.Gray < 0 || r.Gray > 100 {
			return out, ErrUnsupported
		}
		out.Rolling = &r
		for _, model := range []arkcontrol.FoundationModel{r.In, r.Out} {
			p, err := s.modelPrice(ctx, model)
			if err != nil {
				return out, err
			}
			out.Price = ceiling(out.Price, p)
		}
	}
	return out, nil
}
func active(r *arkcontrol.Rolling) bool {
	return r != nil && !(r.Status == "Reverted" && r.Gray == 0) && r.Gray != 100
}
func (s *Service) Preview(ctx context.Context, in Input) (Snapshot, error) {
	if !s.WritesEnabled || !s.Allowed(in.Endpoint) {
		return Snapshot{}, ErrUnsupported
	}
	s.priceMu.Lock()
	s.priceCache = nil
	s.priceMu.Unlock()
	snap, err := s.Read(ctx, in.Endpoint)
	if err != nil {
		return snap, err
	}
	pending, err := s.Store.Pending(ctx, in.Endpoint)
	if err != nil {
		return snap, err
	}
	for _, c := range pending {
		if in.Action != "reconcile" && (c.State != "watching" || in.Action == "start") {
			return snap, ErrConflict
		}
	}
	switch in.Action {
	case "start":
		if !Known(in.Target) || in.Target == endpointModel(snap.Endpoint) || active(snap.Rolling) || (snap.Endpoint.SupportRolling != nil && !*snap.Endpoint.SupportRolling) {
			return snap, ErrUnsupported
		}
		versions, err := s.Cloud.Versions(ctx, in.Target.Name)
		if err != nil {
			return snap, err
		}
		found := false
		for _, v := range versions {
			if v.Version == in.Target.Version && v.Name == in.Target.Name {
				found = true
			}
		}
		if !found {
			return snap, ErrUnsupported
		}
		// Require every accepted Health option and modality, including queued legacy
		// requests whose options can predate the current published configuration.
		for _, op := range contracts.OperationValues() {
			for _, effort := range []string{"minimal", "low", "medium", "high"} {
				p := contracts.ExecutionPolicy{Version: "compatibility", Endpoint: in.Endpoint, ReasoningEffort: effort, TimeoutSeconds: 90, WebSearchEnabled: op == contracts.OperationMealDecision}
				if !Supports(in.Target, op, p) {
					return snap, ErrUnsupported
				}
			}
		}
		p, err := s.modelPrice(ctx, in.Target)
		if err != nil {
			return snap, err
		}
		snap.Price = ceiling(snap.Price, p)
		if err = s.Cloud.PreviewRolling(ctx, in.Endpoint, in.Target); err != nil {
			return snap, err
		}
	case "accept_current":
		if in.Target != (arkcontrol.FoundationModel{}) || active(snap.Rolling) || len(pending) > 0 {
			return snap, ErrConflict
		}
		for _, op := range contracts.OperationValues() {
			p := contracts.ExecutionPolicy{Version: "compatibility", Endpoint: in.Endpoint, ReasoningEffort: "high", TimeoutSeconds: 90, WebSearchEnabled: op == contracts.OperationMealDecision}
			if !Supports(endpointModel(snap.Endpoint), op, p) {
				return snap, ErrUnsupported
			}
		}
	case "reconcile":
		if in.Target != (arkcontrol.FoundationModel{}) || snap.Rolling == nil {
			return snap, ErrUnsupported
		}
		found := false
		for _, c := range pending {
			if c.State == "uncertain" && c.Input.Action == "start" && c.RollingID == "" && c.Before.Endpoint.RollingID != snap.Rolling.ID && snap.Rolling.In == c.Input.Target && snap.Rolling.Out == endpointModel(c.Before.Endpoint) {
				found = true
			}
		}
		if !found {
			return snap, ErrConflict
		}
	case "cancel", "step_back":
		if in.Target != (arkcontrol.FoundationModel{}) || snap.Rolling == nil || (snap.Rolling.Status != "Running" && !(in.Action == "cancel" && snap.Rolling.Status == "Reverting")) || snap.Rolling.Gray >= 100 {
			return snap, ErrUnsupported
		}
		if in.Action == "step_back" && snap.Rolling.Gray <= 0 {
			return snap, ErrUnsupported
		}
	default:
		return snap, ErrUnsupported
	}
	return snap, nil
}
func Same(a, b Snapshot) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func (s *Service) Submit(ctx context.Context, m aiconfig.Mutation, in Input, expected Snapshot) (Command, error) {
	if !s.WritesEnabled || !s.Allowed(in.Endpoint) {
		return Command{}, ErrUnsupported
	}
	if c, err := s.Store.Replay(ctx, m, in); err != nil {
		return Command{}, err
	} else if c != nil {
		return *c, nil
	}
	snap, err := s.Preview(ctx, in)
	if err != nil {
		return Command{}, err
	}
	if !Same(expected, snap) {
		return Command{}, ErrConflict
	}
	var result Command
	err = s.Store.WithLock(ctx, in.Endpoint, func() error {
		var err error
		result, err = s.Store.Enqueue(ctx, m, in, snap)
		return err
	})
	return result, err
}

// Run refreshes billing guards even with writes disabled, and only dispatches
// persisted commands when the explicit native-write switch is enabled.
func (s *Service) Run(ctx context.Context) {
	lastCleanup := time.Time{}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if time.Since(lastCleanup) > time.Hour {
			_, err := s.Store.DB.ExecContext(ctx, "DELETE FROM health_ai_model_attempts WHERE created_at < ? LIMIT 10000", time.Now().UTC().Add(-30*24*time.Hour))
			if err == nil {
				lastCleanup = time.Now()
			}
		}
		var wg sync.WaitGroup
		slots := make(chan struct{}, 4)
		seen := map[string]bool{}
		for _, id := range s.Endpoints {
			if !s.Allowed(id) || seen[id] {
				continue
			}
			seen[id] = true
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
					return
				}
				work, cancel := context.WithTimeout(ctx, 50*time.Second)
				defer cancel()
				_ = s.Tick(work, id)
			}(id)
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Service) Tick(ctx context.Context, id string) error {
	if !s.Allowed(id) {
		return ErrUnsupported
	}
	initial, err := s.Store.Pending(ctx, id)
	if err != nil {
		return err
	}
	if len(initial) == 0 {
		return s.refreshPrice(ctx, id, false)
	}
	return s.Store.WithLock(ctx, id, func() error {
		commands, err := s.Store.Pending(ctx, id)
		if err != nil {
			return err
		}
		for _, c := range commands {
			if c.State == "queued" && s.WritesEnabled {
				if c.Input.Action == "start" {
					waiting, err := s.Store.EarlierAttempts(ctx, c.CreatedAt)
					if err != nil {
						return err
					}
					if waiting {
						continue
					}
				}
				// Replay is never performed after dispatching has reached durable storage.
				snap, err := s.readDispatch(ctx, c)
				if err != nil {
					if errors.Is(err, ErrConflict) || errors.Is(err, ErrUnsupported) {
						c.State = "conflict"
						c.Detail = "云端状态或模型能力已变化"
						if err = s.Store.Save(ctx, c); err != nil {
							return err
						}
					}
					continue
				}
				_ = snap
				if c.Input.Action == "accept_current" {
					c.State = "succeeded"
					c.Detail = "已确认当前模型与费用配置"
					if err = s.Store.Save(ctx, c); err != nil {
						return err
					}
					continue
				}
				if c.Input.Action == "reconcile" {
					if err = s.adopt(ctx, c); err != nil {
						return err
					}
					continue
				}
				c.State = "dispatching"
				if err = s.Store.Save(ctx, c); err != nil {
					return err
				}
				switch c.Input.Action {
				case "start":
					c.RollingID, err = s.Cloud.CreateRolling(ctx, id, c.Input.Target, c.ID)
				case "cancel":
					err = s.Cloud.CancelRolling(ctx, c.RollingID)
				case "step_back":
					err = s.Cloud.RollbackRolling(ctx, c.RollingID)
				}
				if err != nil {
					c.State = "uncertain"
					c.Detail = "火山请求结果待核对；不会自动重复执行"
					var apiErr *arkcontrol.APIError
					if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 408 {
						c.State = "failed"
						c.Detail = apiErr.Code
					}
				} else {
					c.State = "watching"
					c.Detail = "已提交火山，等待云端状态确认"
				}
				if err = s.Store.Save(ctx, c); err != nil {
					return err
				}
			} else if c.State == "dispatching" {
				c.State = "uncertain"
				c.Detail = "服务在调用过程中重启，正在核对云端状态"
				if err = s.Store.Save(ctx, c); err != nil {
					return err
				}
			}
			if c.State == "watching" || c.State == "uncertain" {
				if err = s.reconcile(ctx, c); err != nil {
					return err
				}
			}
		}
		return s.refreshPrice(ctx, id, true)
	})
}
func (s *Service) readDispatch(ctx context.Context, c Command) (Snapshot, error) {
	snap, err := s.Read(ctx, c.Input.Endpoint)
	if err != nil {
		return snap, err
	}
	if c.Input.Action == "start" {
		p, err := s.modelPrice(ctx, c.Input.Target)
		if err != nil {
			return snap, err
		}
		snap.Price = ceiling(snap.Price, p)
	}
	if !Same(snap, c.Before) {
		return snap, ErrConflict
	}
	return snap, nil
}
func (s *Service) reconcile(ctx context.Context, c Command) error {
	if c.RollingID == "" {
		return nil
	} // No documented correlation lookup for a lost Create reply.
	r, err := s.Cloud.Rolling(ctx, c.RollingID)
	if err != nil {
		return err
	}
	if r.EndpointID != c.Input.Endpoint {
		return ErrConflict
	}
	previous := c.State
	if c.Input.Action == "start" {
		if r.In != c.Input.Target || r.Out != endpointModel(c.Before.Endpoint) {
			c.State = "conflict"
			c.Detail = "云端任务与提交的模型不一致"
		} else if r.Status == "Reverted" && r.Gray == 0 {
			c.State = "cancelled"
			c.Detail = "已撤销，新模型流量为 0%"
		} else if r.Gray == 100 {
			ep, err := s.Cloud.Endpoint(ctx, c.Input.Endpoint)
			if err != nil {
				return err
			}
			if endpointModel(ep) == c.Input.Target {
				c.State = "succeeded"
				c.Detail = "新模型已承接 100% 流量"
			}
		}
	} else if c.Input.Action == "cancel" && r.Status == "Reverted" && r.Gray == 0 {
		c.State = "succeeded"
		c.Detail = "已撤销，新模型流量为 0%"
	} else if c.Input.Action == "step_back" && c.Before.Rolling != nil && r.Gray < c.Before.Rolling.Gray {
		c.State = "succeeded"
		c.Detail = "已观察到灰度比例回退"
	}
	if previous != c.State {
		return s.Store.Save(ctx, c)
	}
	return nil
}
func (s *Service) refreshPrice(ctx context.Context, id string, locked bool) error {
	snap, readErr := s.Read(ctx, id)
	save := func() error {
		old, err := s.Store.PriceState(ctx, id)
		if err != nil {
			return err
		}
		if readErr != nil {
			if old != nil && errors.Is(readErr, ErrUnsupported) {
				old.Blocked = true
				return s.Store.SavePrice(ctx, id, *old)
			}
			return readErr
		}
		if old == nil {
			old = &PriceState{Models: []arkcontrol.FoundationModel{endpointModel(snap.Endpoint)}}
		}
		known := func(m arkcontrol.FoundationModel) bool {
			for _, v := range old.Models {
				if v == m {
					return true
				}
			}
			return false
		}
		old.Blocked = old.Drift || !known(endpointModel(snap.Endpoint))
		if active(snap.Rolling) && (!known(snap.Rolling.In) || !known(snap.Rolling.Out)) {
			old.Blocked = true
		}
		old.Price = ceiling(old.Price, snap.Price)
		old.SyncedAt = time.Now().UTC()
		return s.Store.SavePrice(ctx, id, *old)
	}
	if locked {
		return save()
	}
	return s.Store.WithLock(ctx, id, save)
}

// Reconcile is an explicitly confirmed local association, never a retried cloud
// mutation. The administrator sees both models and the native ID in the preview.
func (s *Service) adopt(ctx context.Context, command Command) error {
	commands, err := s.Store.Pending(ctx, command.Input.Endpoint)
	if err != nil {
		return err
	}
	for _, old := range commands {
		if old.ID == command.ID || old.Input.Action != "start" || old.Input.Target != command.Before.Rolling.In || endpointModel(old.Before.Endpoint) != command.Before.Rolling.Out {
			continue
		}
		if old.State == "uncertain" && old.RollingID == "" {
			old.RollingID = command.Before.Rolling.ID
			old.State = "watching"
			old.Detail = "管理员已核对并关联云端任务"
			if err = s.Store.Save(ctx, old); err != nil {
				return err
			}
		}
		if old.RollingID == command.Before.Rolling.ID {
			command.State = "succeeded"
			command.Detail = "云端任务已关联，继续查询状态"
			return s.Store.Save(ctx, command)
		}
	}
	return ErrConflict
}
