package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/jobs"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/provider/ark"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/storage/redisstore"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	platform, err := config.LoadWorkerPlatform()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	database, err := mysqlstore.Open(ctx, platform.DatabaseDSN)
	if err != nil {
		return err
	}
	defer database.Close()
	redisClient, err := redisstore.Open(ctx, platform.RedisURL)
	if err != nil {
		return err
	}
	defer func() { _ = redisClient.Close() }()
	cipher, err := mysqlstore.NewPayloadCipher(platform.PayloadEncryptionKey)
	if err != nil {
		return err
	}
	tosStore, err := media.NewTOSStore(platform.TOS)
	if err != nil {
		return err
	}
	workers := make(map[appregistry.AppID]*jobs.Worker)
	for _, app := range platform.Apps {
		if app.Registry.ID != appregistry.Health {
			continue
		}
		appID := string(app.Registry.ID)
		store := mysqlstore.NewJobRepository(database, cipher, appID)
		provider := ark.New(app.Ark, http.DefaultClient, tosStore)
		managedReconciler := redisstore.NewQuotaLimiter(redisClient, app.Quota, appID)
		freeRecognitionReconciler := redisstore.NewQuotaLimiter(redisClient, app.FreeRecognitionQuota, appID)
		reconciler := quota.NewRoutedTokenReconciler(managedReconciler, freeRecognitionReconciler)
		workers[app.Registry.ID] = jobs.NewWorker(store, provider, reconciler)
	}
	if len(workers) == 0 {
		return errors.New("no asynchronous application workers are configured")
	}
	router := newWorkerRouter(platform.WorkerSecret, workers, logger)

	server := &http.Server{
		Addr:              ":" + platform.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * time.Hour,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	errChannel := make(chan error, 1)
	go func() { errChannel <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	case err := <-errChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
