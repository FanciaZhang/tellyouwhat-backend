package airollout

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tellyouwhat/backend/internal/arkcontrol"
	"github.com/tellyouwhat/backend/internal/contracts"
)

var ErrChecking = errors.New("model compatibility check in progress")

type CheckError struct {
	Stage   string
	Message string
}

func (e *CheckError) Error() string       { return e.Message }
func (e *CheckError) Unwrap() error       { return ErrUnsupported }
func blocked(stage, message string) error { return &CheckError{stage, message} }

type ProtocolProbe interface {
	Probe(context.Context, string, contracts.Operation, contracts.ExecutionPolicy) error
}
type checkResult struct {
	pending bool
	err     error
	at      time.Time
}
type Compatibility struct {
	Probe   ProtocolProbe
	mu      sync.Mutex
	results map[string]checkResult
	slots   chan struct{}
}

func (c *Compatibility) Check(v arkcontrol.Version, r Requirement) error {
	if c == nil || c.Probe == nil {
		return blocked("compatibility", "尚未配置模型协议检查服务")
	}
	p, _ := contracts.PolicyFor(r.Operation)
	kind := "text"
	if _, ok := p.AllowedMedia["image"]; ok {
		kind = "image"
	}
	if _, ok := p.AllowedMedia["audio"]; ok {
		kind = "audio"
	}
	key := fmt.Sprintf("%s/%s/%s/%s/%t", CatalogVersion, v.ModelID, kind, r.Effort, r.Search)
	c.mu.Lock()
	defer c.mu.Unlock()
	if result, ok := c.results[key]; ok {
		if result.pending {
			return ErrChecking
		}
		ttl := 6 * time.Hour
		if result.err != nil {
			ttl = time.Minute
		}
		if time.Since(result.at) < ttl {
			return result.err
		}
	}
	if c.results == nil {
		c.results = map[string]checkResult{}
	}
	if c.slots == nil {
		c.slots = make(chan struct{}, 2)
	}
	// Bounded admission prevents browsing many models from queuing unbounded work.
	select {
	case c.slots <- struct{}{}:
	default:
		return ErrChecking
	}
	c.results[key] = checkResult{pending: true}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
		defer cancel()
		err := c.Probe.Probe(ctx, v.ModelID, r.Operation, r.Policy(v.ModelID))
		if err != nil {
			err = blocked("compatibility", err.Error())
		}
		c.mu.Lock()
		c.results[key] = checkResult{err: err, at: time.Now()}
		<-c.slots
		c.mu.Unlock()
	}()
	return ErrChecking
}
func (s *Service) CheckModel(ctx context.Context, model arkcontrol.FoundationModel, requirements []Requirement) error {
	versions, err := s.Cloud.Versions(ctx, model.Name)
	if err != nil {
		return err
	}
	var selected *arkcontrol.Version
	for _, v := range versions {
		if v.Name == model.Name && v.Version == model.Version {
			value := v
			selected = &value
			break
		}
	}
	if selected == nil {
		return blocked("version", "所选模型版本已不可用，请刷新版本列表")
	}
	if selected.Status != "" && selected.Status != "Published" {
		return blocked("version", "此版本尚未发布，请选择已发布版本")
	}
	// Only explicit non-language task metadata rules a model out. Unrecognized
	// or missing metadata remains eligible for a real protocol probe.
	for _, domain := range selected.Domains {
		switch domain {
		case "T2I", "I2I", "T2V", "I2V", "Embedding", "EMBEDDING", "TTS", "ASR":
			return blocked("compatibility", "此模型用于其他任务类型；当前功能需要理解输入并返回结构化结果，请选择语言或视觉理解模型")
		}
	}
	waiting := false
	for _, r := range requirements {
		if Supports(model, r.Operation, r.Policy("ep-check")) {
			continue
		}
		if selected.ModelID == "" {
			return blocked("compatibility", "火山未返回可验证的模型 ID，请刷新版本列表")
		}
		err := s.Compatibility.Check(*selected, r)
		if errors.Is(err, ErrChecking) {
			waiting = true
			continue
		}
		if err != nil {
			return err
		}
	}
	if waiting {
		return ErrChecking
	}
	return nil
}
