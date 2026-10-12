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
	if _, err := repository.PendingReconciliations(startup, time.Now().UTC(), 1); err != nil {
		return errors.New("album reconciliation schema")
	}
	if err := repository.CheckLibrarySchema(startup); err != nil {
		return errors.New("album library schema")
	}
	if err := repository.CheckPhotoSchema(startup); err != nil {
		return errors.New("album photo processing schema")
	}
	if err := repository.CheckVideoSchema(startup); err != nil {
		return errors.New("album video processing schema")
	}
	if err := repository.CheckDerivedSchema(startup); err != nil {
		return errors.New("album derived settlement schema")
	}
	if err := repository.CheckLivePairSchema(startup); err != nil {
		return errors.New("album live pair schema")
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
	livePairs, err := albums.NewLivePairWorker(repository, service, config.BatchSize)
	if err != nil {
		return errors.New("live pair worker configuration")
	}
	multipart, err := albums.NewMultipartService(repository, objects, time.Now)
	if err != nil {
		return errors.New("multipart service configuration")
	}
	reconciler, err := albums.NewReconciliationWorker(repository, multipart, config.BatchSize)
	if err != nil {
		return errors.New("reconciliation worker configuration")
	}
	finalizer, err := albums.NewDerivedFinalizationWorker(repository, config.BatchSize, time.Now)
	if err != nil {
		return errors.New("derived settlement worker configuration")
	}
	var cleanup *albums.DerivedCleanupWorker
	if config.DerivedCleanup {
		cleanup, err = albums.NewDerivedCleanupWorker(repository, objects, config.BatchSize, time.Now)
		if err != nil {
			return errors.New("derived cleanup worker configuration")
		}
	}
	var videoWorker *albums.VideoWorker
	if config.VideoProcessing {
		videoWorker, err = albums.NewVideoWorker(repository, service, config.VideoQueueID, config.BatchSize)
		if err != nil {
			return errors.New("video worker configuration")
		}
	}
	var photoWorker *albums.PhotoWorker
	if config.PhotoProcessing {
		photoWorker, err = albums.NewPhotoWorker(repository, service, config.BatchSize)
		if err != nil {
			return errors.New("photo worker configuration")
		}
	}
	runCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	workerCount := 4
	if config.PhotoProcessing {
		workerCount++
	}
	if config.VideoProcessing {
		workerCount++
	}
	if cleanup != nil {
		workerCount++
	}
	finished := make(chan error, workerCount)
	go func() {
		finished <- livePairs.Run(runCtx, config.PollInterval, func(batch albums.LivePairBatch, available bool) {
			if !available {
				logger.Error("album live pair queue unavailable")
			}
			if batch.Verified+batch.Failed > 0 {
				logger.Info("album live pair batch", "verified", batch.Verified, "failed", batch.Failed, "contended", batch.Contended)
			}
		})
	}()
	go func() {
		finished <- finalizer.Run(runCtx, config.PollInterval, func(batch albums.DerivedFinalizationBatch, available bool) {
			if !available {
				logger.Error("album derived settlement queue unavailable")
			}
			if batch.Closed+batch.Failed > 0 {
				logger.Info("album derived settlement batch", "closed", batch.Closed, "held", batch.Held, "deferred", batch.Deferred, "failed", batch.Failed, "released_bytes", batch.ReleasedBytes)
			}
		})
	}()
	if cleanup != nil {
		go func() {
			finished <- cleanup.Run(runCtx, config.PollInterval, func(batch albums.DerivedCleanupBatch, available bool) {
				if !available {
					logger.Error("album derived cleanup queue unavailable")
				}
				if batch.Recovered+batch.Retired+batch.Pruned+batch.Failed > 0 {
					logger.Info("album derived cleanup batch", "recovered", batch.Recovered, "retired", batch.Retired, "pruned", batch.Pruned, "waiting", batch.Waiting, "failed", batch.Failed, "contended", batch.Contended)
				}
			})
		}()
	}
	if config.VideoProcessing {
		go func() {
			finished <- videoWorker.Run(runCtx, config.PollInterval, func(batch albums.VideoJobBatch, available bool) {
				if !available {
					logger.Error("album video queue unavailable")
				}
				if batch.Stored+batch.Failed+batch.Contended > 0 {
					logger.Info("album video batch", "stored", batch.Stored, "waiting", batch.Waiting, "failed", batch.Failed, "contended", batch.Contended)
				}
			})
		}()
	}
	if config.PhotoProcessing {
		go func() {
			finished <- photoWorker.Run(runCtx, config.PollInterval, func(batch albums.PhotoJobBatch, available bool) {
				if !available {
					logger.Error("album photo queue unavailable")
				}
				if batch.Stored+batch.Failed+batch.Contended > 0 {
					logger.Info("album photo batch", "stored", batch.Stored, "failed", batch.Failed, "contended", batch.Contended)
				}
			})
		}()
	}
	go func() {
		finished <- reconciler.Run(runCtx, config.PollInterval, func(batch albums.ReconciliationBatch, available bool) {
			if !available {
				logger.Error("album reconciliation queue unavailable")
			}
			if batch.Initialized+batch.Assembled+batch.Failed+batch.Contended > 0 {
				logger.Info("album reconciliation batch", "initialized", batch.Initialized, "assembled", batch.Assembled, "failed", batch.Failed, "contended", batch.Contended)
			}
		})
	}()
	logger.Info("album workers started", "photo_processing", config.PhotoProcessing, "video_processing", config.VideoProcessing, "derived_cleanup", config.DerivedCleanup)
	available := true
	go func() {
		finished <- worker.Run(runCtx, func(batch albums.VerificationBatch, queueAvailable bool) {
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
	}()
	first := <-finished
	stopWorkers()
	for remaining := 1; remaining < workerCount; remaining++ {
		<-finished
	}
	return first
}
