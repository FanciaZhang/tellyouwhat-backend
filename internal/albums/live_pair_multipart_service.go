package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type multipartRecoveryProvider interface {
	MultipartProvider
	abortPurgeUpload(context.Context, string, purgeTarget, string) error
}

// LivePairMultipartService uses the original-upload COS transport without
// representing derived pairs as original backups or reserving quota twice.
type LivePairMultipartService struct {
	repository *MySQLUploads
	provider   multipartRecoveryProvider
	now        func() time.Time
}

func newLivePairMultipartService(r *MySQLUploads, p multipartRecoveryProvider, now func() time.Time) (*LivePairMultipartService, error) {
	if r == nil || p == nil || now == nil {
		return nil, ErrInvalidManifest
	}
	return &LivePairMultipartService{r, p, now}, nil
}
func liveMultipartResource(u LivePairUpload, part string) (Resource, error) {
	r, err := u.resource(part)
	return Resource{ID: part, SizeBytes: r.SizeBytes, SHA256: r.SHA256}, err
}
func liveMultipartSession(u LivePairUpload, part string) (MultipartSession, error) {
	r, err := u.resource(part)
	if err != nil {
		return MultipartSession{}, err
	}
	l, err := NewMultipartLayout(r.SizeBytes)
	return MultipartSession{part, l.SizeBytes, l.PartSize, l.PartCount}, err
}
func (s *LivePairMultipartService) Initialize(ctx context.Context, owner, pair, part string) (MultipartSession, error) {
	u, m, fresh, err := s.repository.beginLiveMultipart(ctx, owner, pair, part, s.now().UTC())
	if err != nil {
		return MultipartSession{}, err
	}
	if fresh {
		key, _ := u.key(part, true)
		provider, callErr := s.provider.InitiateStagingMultipart(ctx, key)
		if callErr != nil || !validCOSMultipartID(provider) {
			provider = ""
		}
		persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		m, err = s.repository.resolveLiveMultipart(persist, owner, m, provider, s.now().UTC())
		stop()
		if err != nil {
			return MultipartSession{}, errors.Join(ErrMultipartPending, err)
		}
	}
	if m.State != "ready" && m.State != "assembled" {
		return MultipartSession{}, ErrMultipartPending
	}
	if err = ctx.Err(); err != nil {
		return MultipartSession{}, err
	}
	if !s.now().Before(u.ExpiresAt) {
		return MultipartSession{}, ErrExpired
	}
	return liveMultipartSession(u, part)
}
func (s *LivePairMultipartService) Part(ctx context.Context, owner, pair, part string, number int) (MultipartPartGrant, error) {
	u, m, err := s.repository.liveMultipart(ctx, owner, pair, part)
	if err != nil {
		return MultipartPartGrant{}, err
	}
	r, _ := liveMultipartResource(u, part)
	layout, err := NewMultipartLayout(r.SizeBytes)
	if err != nil {
		return MultipartPartGrant{}, err
	}
	if _, _, err = layout.Part(number); err != nil {
		return MultipartPartGrant{}, err
	}
	var grant MultipartPartGrant
	err = s.repository.authorizeLiveMultipart(ctx, owner, m, s.now, func(current LivePairUpload, expiry time.Time) error {
		key, _ := current.key(part, true)
		var err error
		grant, err = s.provider.AuthorizePart(ctx, key, m.ProviderID, r, number, expiry)
		return err
	})
	if err != nil {
		return MultipartPartGrant{}, err
	}
	return grant, nil
}
func (s *LivePairMultipartService) Status(ctx context.Context, owner, pair, part string) (MultipartStatus, error) {
	var m livePairMultipart
	u, err := s.repository.mutateLivePair(ctx, owner, pair, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		if _, err := u.resource(part); err != nil {
			return u, err
		}
		if err := requireVisibleVersion(ctx, tx, owner, u.VersionID); err != nil {
			return u, err
		}
		var err error
		m, err = lockLiveMultipart(ctx, tx, pair, part)
		return u, err
	})
	if err != nil {
		return MultipartStatus{}, err
	}
	result := MultipartStatus{part, "pending", []MultipartUploadedPart{}}
	if m.State == "assembled" {
		result.State = "assembled"
		return result, nil
	}
	if m.State != "ready" {
		return result, nil
	}
	if u.State != "uploading" || !s.now().Before(u.ExpiresAt) {
		return MultipartStatus{}, ErrExpired
	}
	key, _ := u.key(part, true)
	r, _ := u.resource(part)
	parts, err := s.provider.ListStagingMultipartParts(ctx, key, m.ProviderID, r.SizeBytes)
	if err != nil {
		return MultipartStatus{}, err
	}
	result.State = "uploading"
	if parts != nil {
		result.Parts = parts
	}
	return result, nil
}

func (s *LivePairMultipartService) Complete(ctx context.Context, owner, pair, part string) (MultipartAssembly, error) {
	u, m, fresh, err := s.repository.claimLiveMultipartOperation(ctx, owner, pair, part, false, s.now().UTC())
	if err != nil {
		return MultipartAssembly{}, err
	}
	if fresh {
		key, _ := u.key(part, true)
		r, _ := u.resource(part)
		version, callErr := s.provider.CompleteStagingMultipart(ctx, key, m.ProviderID, r.SizeBytes)
		state := "assembled"
		if callErr != nil || !concreteCOSVersion(version) {
			version = ""
			state = "completion_unknown"
			if !errors.Is(callErr, ErrCOSMultipartUnknown) && (errors.Is(callErr, ErrIncompleteBackup) || errors.Is(callErr, ErrCOSOperation)) {
				state = "ready"
			}
		}
		persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		m, err = s.repository.resolveLiveMultipartOperation(persist, owner, m, state, version, s.now().UTC())
		stop()
		if err != nil {
			return MultipartAssembly{}, errors.Join(ErrMultipartPending, err)
		}
		if state == "ready" {
			return MultipartAssembly{}, callErr
		}
	}
	if m.State != "assembled" {
		return MultipartAssembly{}, ErrMultipartPending
	}
	if err = ctx.Err(); err != nil {
		return MultipartAssembly{}, err
	}
	return MultipartAssembly{part, "assembled"}, nil
}

// Trusted recovery: only reads an already-issued operation; never initializes or
// completes again. Direct PUTs are excluded before this protocol starts.
func (s *LivePairMultipartService) Reconcile(ctx context.Context, owner, pair, part string) error {
	u, m, err := s.repository.liveMultipart(ctx, owner, pair, part)
	if err != nil {
		return err
	}
	key, _ := u.key(part, true)
	switch m.State {
	case "initiating", "unknown":
		provider, err := s.provider.FindStagingMultipart(ctx, key)
		if err != nil {
			return err
		}
		if !validCOSMultipartID(provider) {
			return ErrMultipartPending
		}
		persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_, err = s.repository.resolveLiveMultipart(persist, owner, m, provider, s.now().UTC())
		return err
	case "completing", "completion_unknown":
		r, _ := liveMultipartResource(u, part)
		version, err := s.provider.VerifyStagingResource(ctx, key, r)
		if err != nil {
			return err
		}
		persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_, err = s.repository.resolveLiveMultipartOperation(persist, owner, m, "assembled", version, s.now().UTC())
		return err
	case "ready", "assembled", "aborted":
		return nil
	default:
		return ErrMultipartPending
	}
}

// Only a deletion-accepted account can stop a ready upload. An in-flight Complete
// must be reconciled, never guessed terminated from Abort or a missing session.
func (s *LivePairMultipartService) AbortForDeletion(ctx context.Context, owner, pair, part string) error {
	u, m, execute, err := s.repository.claimLiveMultipartOperation(ctx, owner, pair, part, true, s.now().UTC())
	if err != nil {
		return err
	}
	if m.State == "aborted" || m.State == "assembled" {
		return nil
	}
	if !execute {
		return ErrMultipartPending
	}
	key, _ := u.key(part, true)
	if err = s.provider.abortPurgeUpload(ctx, owner, purgeTarget{key}, m.ProviderID); err != nil {
		return err
	}
	persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	_, err = s.repository.resolveLiveMultipartOperation(persist, owner, m, "aborted", "", s.now().UTC())
	return err
}
