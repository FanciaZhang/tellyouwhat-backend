package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/albums"
)

func TestInvalidDatabaseFailureIsSanitized(t *testing.T) {
	err := run(context.Background(), albums.WorkerConfig{DatabaseDSN: "private-secret@malformed("}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || err.Error() != "database connection" || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("unsafe database failure %v", err)
	}
}
