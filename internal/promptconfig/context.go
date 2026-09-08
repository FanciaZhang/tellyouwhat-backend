package promptconfig

import "context"

type contextKey struct{}

// WithRevision pins the trusted configuration for one execution and its retries.
func WithRevision(ctx context.Context, r Revision) context.Context {
	return context.WithValue(ctx, contextKey{}, clone(r))
}
func FromContext(ctx context.Context) (Revision, bool) {
	r, ok := ctx.Value(contextKey{}).(Revision)
	return clone(r), ok
}
func Freeze(ctx context.Context, c *Cache) (context.Context, error) {
	if _, ok := FromContext(ctx); ok {
		return ctx, nil
	}
	r, err := c.Current("journal")
	if err != nil {
		return ctx, err
	}
	return WithRevision(ctx, r), nil
}
