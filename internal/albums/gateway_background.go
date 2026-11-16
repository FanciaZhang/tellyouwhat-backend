package albums

import (
	"context"

	"github.com/tellyouwhat/backend/internal/lifecycle"
)

func (g *Gateway) runBackgroundBatch(ctx context.Context, work func()) bool {
	return runGatewayBatch(ctx, func() {
		if g.admitsActiveDatabase(ctx) {
			work()
		}
	})
}

// Count claims through settlement so a release cannot seal an API with work in flight.
func runGatewayBatch(ctx context.Context, work func()) bool {
	for lifecycle.Wait(ctx) {
		done, admitted := lifecycle.Begin(ctx)
		if !admitted {
			continue
		}
		defer done()
		work()
		return true
	}
	return false
}
