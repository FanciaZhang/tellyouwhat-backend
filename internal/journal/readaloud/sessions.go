package readaloud

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

var (
	ErrSessionBusy          = errors.New("read-aloud session capacity reached")
	ErrAudioUnavailable     = errors.New("read-aloud audio is no longer recoverable")
	ErrVoiceChanged         = errors.New("read-aloud voice is no longer available with these settings")
	ErrConsentRequired      = errors.New("read-aloud consent required")
	ErrSubscriptionRequired = errors.New("read-aloud subscription required")
	ErrSubscriptionSync     = errors.New("read-aloud subscription period unavailable")
)

const (
	maximumSessions      = 32
	maximumOwnerSessions = 2
	maximumBufferedBytes = 64 << 20
	audioFrameBytes      = 16 << 10
	maximumReadFrames    = 16
)

type SessionRepository interface {
	ReservationRepository
	Get(context.Context, Subject, string) (RequestRecord, error)
	Usage(context.Context, Grant, time.Time) (CharacterUsage, error)
}
type CatalogReader interface {
	Current(context.Context) (CatalogRevision, error)
}
type SessionCommand struct {
	Synthesis
	VoiceRevision string
	Purpose       string
	AccessKey     string
}
type SessionInfo struct {
	ID              string    `json:"id"`
	Purpose         string    `json:"purpose"`
	State           string    `json:"state"`
	AudioBytes      int       `json:"audioBytes"`
	LastSequence    int       `json:"lastSequence"`
	BufferAvailable bool      `json:"bufferAvailable"`
	ExpiresAt       time.Time `json:"expiresAt"`
	Problem         string    `json:"problem,omitempty"`
	Timings         []Timing  `json:"-"`
	TimingRevision  int       `json:"-"`
}
type AudioFrame struct {
	Sequence int    `json:"sequence"`
	Offset   int    `json:"offset"`
	PCM      []byte `json:"pcm"`
}
type audioSession struct {
	info         SessionInfo
	subject      Subject
	accessKey    string
	hash         [32]byte
	pcm          []byte
	changed      chan struct{}
	initialized  chan struct{}
	initialError error
	cancel       context.CancelFunc
	createdAt    time.Time
}

// Only bounded, short-lived audio lives here. The durable repository owns
// idempotency and accounting; a restart can never start a paid request again.
type SessionService struct {
	store     SessionRepository
	provider  Synthesizer
	catalog   CatalogReader
	authorize func(context.Context, Subject, string) error
	now       func() time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	sessions  map[string]*audioSession
	bytes     int
	workers   int
	closed    bool
	jobs      sync.WaitGroup
}

func NewSessionService(parent context.Context, store SessionRepository, provider Synthesizer, catalog CatalogReader, authorize func(context.Context, Subject, string) error, now func() time.Time) (*SessionService, error) {
	if parent == nil || store == nil || provider == nil || catalog == nil || authorize == nil {
		return nil, ErrInput
	}
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(parent)
	return &SessionService{store: store, provider: provider, catalog: catalog, authorize: authorize, now: now, ctx: ctx, cancel: cancel, sessions: map[string]*audioSession{}}, nil
}
func (s *SessionService) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
		for id, session := range s.sessions {
			s.removeLocked(id, session)
		}
	}
	s.mu.Unlock()
}
func (s *SessionService) Run() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	defer func() { s.Close(); s.jobs.Wait() }()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			s.expireLocked()
			s.mu.Unlock()
		}
	}
}
func (s *SessionService) signalLocked(session *audioSession) {
	close(session.changed)
	session.changed = make(chan struct{})
}
func (s *SessionService) removeLocked(id string, session *audioSession) {
	session.cancel()
	clear(session.pcm)
	s.bytes -= session.info.AudioBytes
	session.pcm = nil
	session.info.Timings = nil
	session.info.AudioBytes = 0
	session.info.LastSequence = 0
	session.info.BufferAvailable = false
	delete(s.sessions, id)
	s.signalLocked(session)
}
func (s *SessionService) expireLocked() {
	for id, session := range s.sessions {
		if !s.now().Before(session.info.ExpiresAt) {
			s.removeLocked(id, session)
		}
	}
}
func (s *SessionService) makeRoomLocked(subject Subject) bool {
	s.expireLocked()
	if s.workers >= maximumSessions {
		return false
	}
	for {
		owners := 0
		var oldest *audioSession
		for _, item := range s.sessions {
			if item.subject == subject {
				owners++
			}
		}
		if owners < maximumOwnerSessions && len(s.sessions) < maximumSessions {
			return true
		}
		for _, item := range s.sessions {
			if owners >= maximumOwnerSessions && item.subject != subject {
				continue
			}
			if item.info.State == "preparing" || item.info.State == "running" {
				continue
			}
			if oldest == nil || item.createdAt.Before(oldest.createdAt) {
				oldest = item
			}
		}
		if oldest == nil {
			return false
		}
		s.removeLocked(oldest.info.ID, oldest)
	}
}

func (s *SessionService) Create(ctx context.Context, grant Grant, command SessionCommand) (SessionInfo, error) {
	if !grant.Valid(s.now()) || grant.Purpose != command.Purpose {
		return SessionInfo{}, ErrInput
	}
	if command.AccessKey == "" {
		command.AccessKey = grant.Subject.ID
	}
	accessSubject := Subject{AppID: grant.Subject.AppID, ID: command.AccessKey}
	if !accessSubject.Valid() {
		return SessionInfo{}, ErrInput
	}
	catalog, err := s.catalog.Current(ctx)
	if err != nil {
		return SessionInfo{}, ErrCatalog
	}
	if catalog.ValidatePublication() != nil {
		return SessionInfo{}, ErrCatalog
	}
	index := slices.IndexFunc(catalog.Voices, func(v Voice) bool { return v.ID == command.VoiceID })
	if index < 0 {
		return SessionInfo{}, ErrVoiceChanged
	}
	voice := catalog.Voices[index]
	if command.Style == "" {
		command.Style = "natural"
	}
	if command.Purpose == "preview" {
		if command.Text != "" {
			return SessionInfo{}, ErrInput
		}
		command.Text = voice.ProbeText
	}
	if !voice.Allows(command.Synthesis) || voice.policyHash() != command.VoiceRevision {
		return SessionInfo{}, ErrVoiceChanged
	}
	request, err := NewUnitRequest(command.Synthesis, command.VoiceRevision, command.Purpose)
	if err != nil {
		return SessionInfo{}, err
	}
	command.RequestID = request.ID
	if err = s.authorize(ctx, accessSubject, command.Purpose); err != nil {
		return SessionInfo{}, err
	}
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil {
		s.mu.Unlock()
		return SessionInfo{}, context.Canceled
	}
	if previous := s.sessions[request.ID]; previous != nil {
		if previous.subject != grant.Subject {
			s.mu.Unlock()
			return SessionInfo{}, ErrRequestNotFound
		}
		if previous.hash != request.Hash {
			s.mu.Unlock()
			return SessionInfo{}, ErrRequestConflict
		}
		ready := previous.initialized
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return SessionInfo{}, ctx.Err()
		case <-ready:
		}
		s.mu.Lock()
		info, err := previous.info, previous.initialError
		s.mu.Unlock()
		return info, err
	}
	if !s.makeRoomLocked(grant.Subject) {
		s.mu.Unlock()
		return SessionInfo{}, ErrSessionBusy
	}
	work, cancel := context.WithTimeout(s.ctx, time.Minute)
	item := &audioSession{info: SessionInfo{ID: request.ID, Purpose: command.Purpose, State: "preparing", BufferAvailable: true, ExpiresAt: s.now().Add(3 * time.Minute)}, subject: grant.Subject, accessKey: command.AccessKey, hash: request.Hash, changed: make(chan struct{}), initialized: make(chan struct{}), cancel: cancel, createdAt: s.now()}
	s.sessions[request.ID] = item
	s.workers++
	s.jobs.Add(1)
	s.mu.Unlock()
	claim, cancelClaim := context.WithTimeout(ctx, 5*time.Second)
	stop := context.AfterFunc(work, cancelClaim)
	record, replay, err := s.store.Begin(claim, grant, request, s.now())
	stop()
	cancelClaim()
	s.mu.Lock()
	if err != nil || replay {
		item.initialError = err
		if replay && err == nil {
			item.info = unbufferedSession(record)
		}
		if s.sessions[request.ID] == item {
			s.removeLocked(request.ID, item)
		}
		close(item.initialized)
		s.workers--
		s.mu.Unlock()
		s.jobs.Done()
		return item.info, err
	}
	close(item.initialized)
	info := item.info
	s.mu.Unlock()
	go func() { defer s.jobs.Done(); s.run(work, grant, command, item, record) }()
	return info, nil
}

func unbufferedSession(r RequestRecord) SessionInfo {
	return SessionInfo{ID: r.ID, Purpose: r.Purpose, State: r.State, ExpiresAt: r.ExpiresAt, Problem: "audio_unavailable"}
}
func (s *SessionService) run(ctx context.Context, grant Grant, command SessionCommand, item *audioSession, record RequestRecord) {
	defer item.cancel()
	provider := synthesisFunc(func(ctx context.Context, input Synthesis, emit func([]byte) error) (SynthesisUsage, error) {
		if err := ctx.Err(); err != nil {
			return SynthesisUsage{UsageKnown: true}, err
		}
		if err := s.authorize(ctx, Subject{AppID: grant.Subject.AppID, ID: command.AccessKey}, command.Purpose); err != nil {
			return SynthesisUsage{UsageKnown: true}, err
		}
		latest, err := s.catalog.Current(ctx)
		if err != nil {
			return SynthesisUsage{UsageKnown: true}, ErrCatalog
		}
		index := slices.IndexFunc(latest.Voices, func(v Voice) bool { return v.ID == input.VoiceID })
		if index < 0 || !latest.Voices[index].Allows(input) || latest.Voices[index].policyHash() != command.VoiceRevision {
			return SynthesisUsage{UsageKnown: true}, ErrVoiceChanged
		}
		written := 0
		input.OnTimings = func(timings []Timing) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			if ctx.Err() != nil || s.sessions[item.info.ID] != item {
				return context.Canceled
			}
			if !validTimings(timings, utf16Length(input.Text)) {
				return ErrIncomplete
			}
			if !slices.Equal(item.info.Timings, timings) {
				item.info.Timings = slices.Clone(timings)
				item.info.TimingRevision++
				s.signalLocked(item)
			}
			return nil
		}
		usage, err := s.provider.Synthesize(ctx, input, func(data []byte) error {
			if e := emit(data); e != nil {
				return e
			}
			written += len(data)
			return nil
		})
		if err == nil && (written == 0 || written%2 != 0 || usage.AudioBytes != written) {
			err = ErrIncomplete
		}
		return usage, err
	})
	account := &AccountSynthesizer{store: s.store, provider: provider, grant: grant, revision: command.VoiceRevision, now: s.now}
	usage, err := account.synthesizeClaimed(ctx, command.Synthesis, record, func(data []byte) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if ctx.Err() != nil || s.sessions[item.info.ID] != item {
			return context.Canceled
		}
		if len(data) > MaximumAudioBytes-item.info.AudioBytes || len(data) > maximumBufferedBytes-s.bytes {
			return ErrSessionBusy
		}
		item.pcm = append(item.pcm, data...)
		item.info.AudioBytes += len(data)
		s.bytes += len(data)
		item.info.LastSequence = item.info.AudioBytes &^ 1
		item.info.State = "running"
		s.signalLocked(item)
		return nil
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workers--
	if s.sessions[item.info.ID] != item {
		return
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && (item.info.AudioBytes == 0 || item.info.AudioBytes%2 != 0 || usage.AudioBytes != item.info.AudioBytes) {
		err = ErrIncomplete
	}
	item.info.State = "succeeded"
	if err != nil {
		item.info.State = "failed"
		item.info.Problem = sessionProblem(err)
		if errors.Is(err, context.Canceled) {
			item.info.State = "cancelled"
		}
	}
	item.info.ExpiresAt = s.now().Add(90 * time.Second)
	s.signalLocked(item)
}

func sessionProblem(err error) string {
	switch {
	case errors.Is(err, ErrConsentRequired):
		return "consent_required"
	case errors.Is(err, ErrAccessUnavailable):
		return "authorization_unavailable"
	case errors.Is(err, ErrVoiceChanged):
		return "voice_changed"
	case errors.Is(err, ErrSubscriptionRequired):
		return "subscription_required"
	case errors.Is(err, costcontrol.ErrBudgetExceeded), errors.Is(err, costcontrol.ErrProtectionActive):
		return "budget_protected"
	case errors.Is(err, costcontrol.ErrConcurrencyExceeded), errors.Is(err, ErrSessionBusy):
		return "service_busy"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "stopped"
	default:
		return "synthesis_unavailable"
	}
}

func (s *SessionService) Get(ctx context.Context, subject Subject, id string) (SessionInfo, error) {
	if !subject.Valid() {
		return SessionInfo{}, ErrInput
	}
	s.mu.Lock()
	s.expireLocked()
	if item := s.sessions[id]; item != nil {
		if item.subject != subject {
			s.mu.Unlock()
			return SessionInfo{}, ErrRequestNotFound
		}
		info := item.info
		s.mu.Unlock()
		return info, nil
	}
	s.mu.Unlock()
	r, err := s.store.Get(ctx, subject, id)
	if err != nil {
		return SessionInfo{}, err
	}
	return unbufferedSession(r), nil
}

// The caller waits on changed or its HTTP context, then resumes at the last
// accepted sequence. Returned frames cannot mutate the protected buffers.
func (s *SessionService) Read(ctx context.Context, subject Subject, id string, after int) (SessionInfo, []AudioFrame, <-chan struct{}, error) {
	if !subject.Valid() {
		return SessionInfo{}, nil, nil, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return SessionInfo{}, nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	item := s.sessions[id]
	if item == nil {
		return SessionInfo{}, nil, nil, ErrAudioUnavailable
	}
	if item.subject != subject {
		return SessionInfo{}, nil, nil, ErrRequestNotFound
	}
	available := len(item.pcm) &^ 1
	if after < 0 || after > available || after%2 != 0 {
		return SessionInfo{}, nil, nil, ErrInput
	}
	frames := []AudioFrame{}
	for offset := after; offset < available && len(frames) < maximumReadFrames; {
		end := min(offset+audioFrameBytes, available)
		frames = append(frames, AudioFrame{Sequence: end, Offset: offset, PCM: slices.Clone(item.pcm[offset:end])})
		offset = end
	}
	info := item.info
	info.Timings = slices.Clone(info.Timings)
	return info, frames, item.changed, nil
}
func (s *SessionService) Cancel(ctx context.Context, subject Subject, id string) (SessionInfo, error) {
	s.mu.Lock()
	if item := s.sessions[id]; item != nil {
		if item.subject != subject {
			s.mu.Unlock()
			return SessionInfo{}, ErrRequestNotFound
		}
		item.info.State = "cancelled"
		item.info.Problem = "stopped"
		s.removeLocked(id, item)
		info := item.info
		s.mu.Unlock()
		return info, nil
	}
	s.mu.Unlock()
	return s.Get(ctx, subject, id)
}
func (s *SessionService) CancelSubject(subject Subject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, item := range s.sessions {
		if item.subject == subject {
			item.info.State = "cancelled"
			item.info.Problem = "consent_revoked"
			s.removeLocked(id, item)
		}
	}
}

func (s *SessionService) CancelAccessKey(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, item := range s.sessions {
		if item.accessKey == key {
			item.info.State = "cancelled"
			item.info.Problem = "consent_revoked"
			s.removeLocked(id, item)
		}
	}
}

func (s *SessionService) Usage(ctx context.Context, grant Grant) (CharacterUsage, error) {
	return s.store.Usage(ctx, grant, s.now())
}
