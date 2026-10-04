package development

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/privacy"
)

const illustrationPrefix = "/v1/journal/illustrations"

func illustrationRoute(r *http.Request) bool {
	if r.URL.Path == illustrationPrefix {
		return r.Method == http.MethodPost
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, illustrationPrefix+"/"), "/")
	return strings.HasPrefix(r.URL.Path, illustrationPrefix+"/") &&
		(len(parts) == 1 && r.Method == http.MethodGet || len(parts) == 2 &&
			(parts[1] == "result" && r.Method == http.MethodGet || parts[1] == "cancel" && r.Method == http.MethodPost))
}

type imageJob struct {
	Task   illustration.Task
	Input  *illustration.Input  `json:",omitempty"`
	Result *illustration.Result `json:",omitempty"`
}

// One private developer process owns this encrypted temporary store. Task
// tombstones survive expiry and restarts so reusing an ID never generates twice.
type IllustrationRuntime struct {
	mu        sync.Mutex
	root      string
	cipher    cipher.AEAD
	generator illustration.ImageGenerator
	now       func() time.Time
	authorize func(context.Context, string) bool
	index     map[string]illustration.Task
}

func NewIllustrationRuntime(root, token string, generator illustration.ImageGenerator, now func() time.Time) (*IllustrationRuntime, error) {
	if root == "" || len(token) < 43 || generator == nil || generator.Model() == "" {
		return nil, illustration.ErrInput
	}
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	// Keep the payload key separate from the revocable connection credential.
	// Rotation changes authenticated owner identities and revokes their delivery,
	// while allowing scheduled expiry to erase old encrypted temporary payloads.
	keyPath := filepath.Join(root, ".payload-key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, writeErr := file.Write(key)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil {
			return nil, writeErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, illustration.ErrInput
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	runtime := &IllustrationRuntime{root: root, cipher: aead, generator: generator, now: now, index: map[string]illustration.Task{}}
	// A running request may have reached the provider before the process died.
	// Preserve its identity, erase private input, and never redispatch it.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".job") {
			continue
		}
		job, err := runtime.loadPath(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		runtime.index[filepath.Join(root, entry.Name())] = job.Task
		if job.Task.State == illustration.TaskRunning {
			job.Task.State = illustration.TaskUncertain
			job.Task.Revision++
			job.Input = nil
			if err = runtime.write(job); err != nil {
				return nil, err
			}
		}
	}
	return runtime, nil
}

func (h *IllustrationRuntime) path(owner, id string) string {
	hash := sha256.Sum256([]byte(owner + ":" + id))
	return filepath.Join(h.root, hex.EncodeToString(hash[:])+".job")
}
func (h *IllustrationRuntime) loadPath(path string) (imageJob, error) {
	plain, err := h.readEncrypted(path)
	if err != nil {
		return imageJob{}, err
	}
	var job imageJob
	err = json.Unmarshal(plain, &job)
	return job, err
}
func (h *IllustrationRuntime) readEncrypted(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 70_000_001))
	if err != nil || len(data) > 70_000_000 {
		return nil, illustration.ErrResult
	}
	n := h.cipher.NonceSize()
	if len(data) < n {
		return nil, illustration.ErrResult
	}
	return h.cipher.Open(nil, data[:n], data[n:], []byte(filepath.Base(path)))
}
func (h *IllustrationRuntime) load(owner, id string) (imageJob, error) {
	job, err := h.loadPath(h.path(owner, id))
	if err != nil {
		return job, err
	}
	if job.Task.OwnerID != owner || job.Task.ID != id || job.Task.AppID != "journal" {
		return imageJob{}, illustration.ErrTaskConflict
	}
	return job, nil
}
func (h *IllustrationRuntime) write(job imageJob) error {
	path := h.path(job.Task.OwnerID, job.Task.ID)
	plain, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if err = h.writeEncrypted(path, plain); err != nil {
		return err
	}
	h.index[path] = job.Task
	return nil
}
func (h *IllustrationRuntime) writeEncrypted(path string, plain []byte) error {
	nonce := make([]byte, h.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	data := h.cipher.Seal(nonce, nonce, plain, []byte(filepath.Base(path)))
	f, err := os.CreateTemp(h.root, ".image-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(h.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return err
	}
	return nil
}

func (h *IllustrationRuntime) expire(job *imageJob) bool {
	if h.now().Before(job.Task.ExpiresAt) {
		return false
	}
	if job.Input == nil && job.Result == nil && job.Task.State == illustration.TaskCancelled {
		return false
	}
	_ = job.Task.Cancel(job.Task.Revision)
	job.Input = nil
	job.Result = nil
	return true
}

func (h *IllustrationRuntime) create(owner string, c illustration.Confirmation, reference []byte) (illustration.Task, error) {
	next, err := illustration.NewTask("journal", owner, h.generator.Model(), c, reference, h.now())
	if err != nil {
		return next, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	existing, err := h.load(owner, c.RequestID)
	if err == nil {
		if !existing.Task.SameRequest(next) {
			return next, illustration.ErrTaskConflict
		}
		return existing.Task, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return next, err
	}
	job := imageJob{Task: next, Input: &illustration.Input{Prompt: c.Prompt, Reference: reference}}
	return next, h.write(job)
}

// Run owns all provider work. An HTTP disconnect cannot cancel an accepted
// task. The persisted running fence precedes dispatch; only queued jobs recover.
func (h *IllustrationRuntime) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		h.mu.Lock()
		paths := make([]string, 0, len(h.index))
		for path := range h.index {
			paths = append(paths, path)
		}
		h.mu.Unlock()
		for _, path := range paths {
			if ctx.Err() != nil {
				return
			}
			h.process(ctx, path)
		}
	}
}

func (h *IllustrationRuntime) process(ctx context.Context, path string) {
	h.mu.Lock()
	if cached, ok := h.index[path]; ok && (cached.State == illustration.TaskCancelled ||
		cached.State != illustration.TaskQueued && h.now().Before(cached.ExpiresAt)) {
		h.mu.Unlock()
		return
	}
	job, err := h.loadPath(path)
	if err != nil {
		h.mu.Unlock()
		return
	}
	if h.expire(&job) {
		_ = h.write(job)
		h.mu.Unlock()
		return
	}
	if job.Task.State != illustration.TaskQueued || job.Input == nil || h.authorize == nil || !h.authorize(ctx, job.Task.OwnerID) {
		h.mu.Unlock()
		return
	}
	if job.Task.Model != h.generator.Model() {
		_ = job.Task.Cancel(job.Task.Revision)
		job.Input = nil
		_ = h.write(job)
		h.mu.Unlock()
		return
	}
	if err = job.Task.Start(job.Task.Revision, h.now()); err != nil {
		h.mu.Unlock()
		return
	}
	if err = h.write(job); err != nil {
		h.mu.Unlock()
		return
	}
	input := *job.Input
	claimed := job.Task
	h.mu.Unlock()
	call, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var result illustration.Result
	var failure error
	if !h.authorize(call, claimed.OwnerID) {
		failure = illustration.NotDispatched{Cause: errors.New("image authorization revoked")}
	}
	if failure == nil {
		result, failure = h.generator.Generate(call, input)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	current, err := h.load(claimed.OwnerID, claimed.ID)
	if err != nil {
		return
	}
	current.Input = nil
	var notDispatched illustration.NotDispatched
	if errors.As(failure, &notDispatched) {
		_ = current.Task.AbortBeforeDispatch()
	} else {
		var output *illustration.Result
		if failure == nil {
			output = &result
		}
		// Cancellation may advance the revision while preserving the claim's charge.
		deliver, err := current.Task.RecordOutcome(current.Task.Revision, output, failure, h.now())
		if err != nil {
			return
		}
		if deliver {
			current.Result = output
		}
	}
	_ = h.write(current)
}

type imageTaskResponse struct {
	ID        string                 `json:"id"`
	VersionID string                 `json:"versionID"`
	State     illustration.TaskState `json:"state"`
	Revision  uint64                 `json:"revision"`
	ExpiresAt time.Time              `json:"expiresAt"`
}

func imageJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
func imageFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		deny(w, 404, "not_found")
	case errors.Is(err, illustration.ErrInput):
		deny(w, 422, "contract_violation")
	case errors.Is(err, illustration.ErrTaskConflict):
		deny(w, 409, "image_task_changed")
	default:
		deny(w, 503, "image_unavailable")
	}
}

func (h *IllustrationRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := r.Context().Value(principalContextKey{}).(attestation.Principal)
	if !ok || p.AppID != "journal" {
		deny(w, 401, "development_access_denied")
		return
	}
	cancellation := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel")
	if !cancellation && (h.authorize == nil || !h.authorize(r.Context(), p.KeyID)) {
		deny(w, 403, "image_access_denied")
		return
	}
	var task illustration.Task
	if r.URL.Path == illustrationPrefix {
		var body struct {
			RequestID       string `json:"requestID"`
			VersionID       string `json:"versionID"`
			Prompt          string `json:"prompt"`
			ConsentRevision string `json:"consentRevision"`
			ReferenceSHA256 string `json:"referenceSHA256"`
			Reference       []byte `json:"reference"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 34_000_000))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || body.RequestID != r.Header.Get("X-Tellyouwhat-Request-ID") || body.ConsentRevision != privacy.JournalIllustrationDocumentVersion {
			deny(w, 422, "contract_violation")
			return
		}
		c := illustration.Confirmation{RequestID: body.RequestID, VersionID: body.VersionID, Prompt: body.Prompt, ConsentRevision: body.ConsentRevision, ReferenceSHA256: body.ReferenceSHA256}
		var err error
		task, err = h.create(p.KeyID, c, body.Reference)
		if err != nil {
			imageFailure(w, err)
			return
		}
	} else {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, illustrationPrefix+"/"), "/")
		id, err := uuid.Parse(parts[0])
		if err != nil || id == uuid.Nil || id.String() != parts[0] {
			deny(w, 422, "contract_violation")
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		job, err := h.load(p.KeyID, id.String())
		if err != nil {
			imageFailure(w, err)
			return
		}
		if h.expire(&job) {
			if err = h.write(job); err != nil {
				imageFailure(w, err)
				return
			}
		}
		if len(parts) == 2 && parts[1] == "cancel" {
			var body struct {
				Revision uint64 `json:"revision"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || body.Revision == 0 {
				deny(w, 422, "contract_violation")
				return
			}
			if err = job.Task.Cancel(body.Revision); err != nil {
				imageFailure(w, err)
				return
			}
			job.Input = nil
			job.Result = nil
			if err = h.write(job); err != nil {
				imageFailure(w, err)
				return
			}
		} else if len(parts) == 2 && parts[1] == "result" {
			if job.Task.State != illustration.TaskSucceeded || job.Result == nil {
				deny(w, 409, "image_task_changed")
				return
			}
			result := job.Result
			imageJSON(w, struct {
				Image  []byte `json:"image"`
				MIME   string `json:"mime"`
				Width  int    `json:"width"`
				Height int    `json:"height"`
			}{result.Image, result.MIME, result.Width, result.Height})
			return
		}
		task = job.Task
	}
	imageJSON(w, imageTaskResponse{task.ID, task.VersionID, task.State, task.Revision, task.ExpiresAt})
}
