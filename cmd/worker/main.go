package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/jobs"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/observability"
	"github.com/tellyouwhat/backend/internal/provider/ark"
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
	serviceConfig, err := config.LoadWorker()
	if err != nil {
		return err
	}
	if serviceConfig.StorageMode != "mysql" {
		return errors.New("distributed worker requires mysql storage")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	database, err := mysqlstore.Open(ctx, serviceConfig.DatabaseDSN)
	if err != nil {
		return err
	}
	defer database.Close()
	redisClient, err := redisstore.Open(ctx, serviceConfig.RedisURL)
	if err != nil {
		return err
	}
	defer func() { _ = redisClient.Close() }()
	payloadCipher, err := mysqlstore.NewPayloadCipher(serviceConfig.PayloadEncryptionKey)
	if err != nil {
		return err
	}
	jobStore := mysqlstore.NewJobRepository(database, payloadCipher)
	tosStore, err := media.NewTOSStore(serviceConfig.TOS)
	if err != nil {
		return err
	}
	provider := ark.New(serviceConfig.Ark, http.DefaultClient, tosStore)
	reconciler := redisstore.NewQuotaLimiter(redisClient, serviceConfig.Quota)
	worker := jobs.NewWorker(jobStore, provider, reconciler)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /internal/jobs/process", func(writer http.ResponseWriter, request *http.Request) {
		provided := []byte(request.Header.Get("X-Health-Worker-Secret"))
		expected := []byte(serviceConfig.WorkerSecret)
		if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, readErr := io.ReadAll(io.LimitReader(request.Body, (8<<10)+1))
		if readErr != nil || len(body) > 8<<10 {
			http.Error(writer, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		var input struct {
			JobID string `json:"jobID"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		decoderErr := decoder.Decode(&input)
		var extra json.RawMessage
		trailingErr := decoder.Decode(&extra)
		if decoderErr != nil || !errors.Is(trailingErr, io.EOF) || input.JobID == "" {
			http.Error(writer, "invalid job", http.StatusUnprocessableEntity)
			return
		}
		if processErr := worker.Process(request.Context(), input.JobID); processErr != nil {
			http.Error(writer, "job failed", http.StatusBadGateway)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{
		Addr:              ":" + serviceConfig.Port,
		Handler:           observability.HTTPLogger(logger, observability.RecoverPanics(logger, mux)),
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
