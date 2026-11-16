package albums

import "context"

func (p *objectTestStore) StartPhotoMultipart(context.Context, PhotoWriteSpec) (string, error) {
	return "photo-fixture-session", nil
}
func (p *objectTestStore) FindPhotoMultipart(context.Context, PhotoWriteSpec) (string, error) {
	return "photo-fixture-session", nil
}
func (p *queuedPhotoProvider) StorePhotoMultipartOnce(ctx context.Context, spec PhotoWriteSpec, _ string, candidate PhotoCandidate, beforeComplete func(context.Context) error) (SealedObject, error) {
	if err := beforeComplete(ctx); err != nil {
		return SealedObject{}, err
	}
	return p.StorePhotoCandidateOnce(ctx, spec, candidate)
}
func (p *queuedPhotoProvider) ReconcilePhotoMultipart(ctx context.Context, spec PhotoWriteSpec, _ string, beforeComplete func(context.Context) error) (SealedObject, error) {
	if err := beforeComplete(ctx); err != nil {
		return SealedObject{}, err
	}
	return p.ReconcilePhotoCandidate(ctx, spec)
}
func (p *journalProvider) StorePhotoMultipartOnce(ctx context.Context, spec PhotoWriteSpec, _ string, candidate PhotoCandidate, beforeComplete func(context.Context) error) (SealedObject, error) {
	if err := beforeComplete(ctx); err != nil {
		return SealedObject{}, err
	}
	return p.StorePhotoCandidateOnce(ctx, spec, candidate)
}
func (p *journalProvider) ReconcilePhotoMultipart(ctx context.Context, spec PhotoWriteSpec, _ string, beforeComplete func(context.Context) error) (SealedObject, error) {
	if err := beforeComplete(ctx); err != nil {
		return SealedObject{}, err
	}
	return p.ReconcilePhotoCandidate(ctx, spec)
}

func allowPhotoCompletionFixture(context.Context) error { return nil }

func (*cleanupProviderFixture) FindPhotoMultipart(context.Context, PhotoWriteSpec) (string, error) {
	return "", ErrPhotoWriteUncertain
}
func (*cleanupProviderFixture) AbortPhotoMultipart(context.Context, PhotoWriteRecord) error {
	return ErrPhotoWriteUncertain
}
