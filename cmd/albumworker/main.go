package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tellyouwhat/backend/internal/albums"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, err := albums.LoadWorkerConfig(os.Getenv)
	if err != nil {
		logger.Error("album worker configuration rejected", "reason", err.Error())
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, config, logger); err != nil && !errors.Is(err, context.Canceled) {
		// Database/provider errors may contain DSNs, keys, or signed URLs.
		logger.Error("album worker stopped", "stage", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, config albums.WorkerConfig, logger *slog.Logger) error {
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := mysqlstore.Open(startup, config.DatabaseDSN)
	if err != nil {
		return errors.New("database connection")
	}
	defer db.Close()
	repository := albums.NewMySQLUploads(db)
	if _, err := repository.Pending(startup, time.Now().UTC(), 1); err != nil {
		return errors.New("album database schema")
	}
	objects, err := albums.NewCOSObjects(config.COS)
	if err != nil {
		return errors.New("object storage configuration")
	}
	if err := objects.Check(startup); err != nil {
		return errors.New("object storage versioning check")
	}
	service, err := albums.NewUploadService(repository, objects, config.Policy, time.Now)
	if err != nil {
		return errors.New("upload policy")
	}
	worker, err := albums.NewVerificationWorker(repository, service, config.BatchSize, config.PollInterval)
	if err != nil {
		return errors.New("verification worker configuration")
	}
	logger.Info("album verification worker started")
	available := true
	return worker.Run(ctx, func(batch albums.VerificationBatch, queueAvailable bool) {
		if !queueAvailable && available {
			logger.Error("album queue unavailable")
		}
		if queueAvailable && !available {
			logger.Info("album queue recovered")
		}
		available = queueAvailable
		if batch.Verified+batch.Failed+batch.Contended > 0 {
			logger.Info("album verification batch", "verified", batch.Verified, "failed", batch.Failed, "contended", batch.Contended, "cleanup_required", batch.CleanupRequired)
		}
	})
}
