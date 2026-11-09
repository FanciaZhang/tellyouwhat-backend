package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/journal/recordinghttp"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/lifecycle"
	"github.com/tellyouwhat/backend/internal/privacy"
)

type journalRecordingPrivacy struct {
	store   *voice.RecordingJobStore
	handler *recordinghttp.Handler
	next    privacy.CacheCleaner
}

func (c *journalRecordingPrivacy) DeletePrincipal(ctx context.Context, p attestation.Principal) error {
	if p.AppID != "journal" || p.KeyID == "" {
		return errors.New("recording privacy identity mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.store.DeleteOwner(p.KeyID); err != nil {
		return err
	}
	if c.handler != nil {
		c.handler.ForgetOwner(p.KeyID)
	}
	if c.next != nil {
		return c.next.DeletePrincipal(ctx, p)
	}
	return nil
}

func runRecordingRetention(ctx context.Context, store *voice.RecordingJobStore, group *sync.WaitGroup, logger *slog.Logger) {
	group.Add(1)
	go func() {
		defer group.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				finish, ok := lifecycle.Begin(ctx)
				if !ok {
					continue
				}
				err := store.Cleanup()
				finish()
				if err != nil {
					logger.Error("Journal recording retention cleanup failed")
				}
			}
		}
	}()
}
