package costcontrol

import "context"

type audienceKey struct{}

// Access travels only inside the server. KeyID is never stored in the cost ledger.
// Managed is the access decision made at admission, including trial subscriptions.
type Access struct {
	KeyID   string
	Managed bool
}

func WithAccess(ctx context.Context, keyID string, managed bool) context.Context {
	return context.WithValue(ctx, audienceKey{}, Access{keyID, managed})
}
func AccessFrom(ctx context.Context) (Access, bool) {
	a, ok := ctx.Value(audienceKey{}).(Access)
	return a, ok && a.KeyID != ""
}
