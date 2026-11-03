package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tellyouwhat/backend/internal/albums"
)

func main() {
	probe := flag.Bool("check-ready", false, "check this container's album API readiness")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, err := albums.LoadGatewayConfig(os.Getenv)
	if err != nil {
		logger.Error("album API configuration rejected", "reason", err.Error())
		os.Exit(1)
	}
	if *probe {
		if err := checkReady(config); err != nil {
			logger.Error("album API not ready")
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, config, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("album API stopped", "stage", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, config albums.GatewayConfig, logger *slog.Logger) error {
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	gateway, err := albums.NewGateway(startup, config, nil)
	cancel()
	if err != nil {
		return err
	}
	defer gateway.Close()
	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		return errors.New("album HTTP listener")
	}
	server := &http.Server{Handler: gateway, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 75 * time.Second, MaxHeaderBytes: 32 << 10}
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("album API listening", "environment", config.Environment, "host", config.APIHost)
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("album HTTP server")
		}
		return nil
	case <-ctx.Done():
		gateway.BeginDrain()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := server.Shutdown(shutdown)
		cancel()
		if err != nil {
			_ = server.Close()
		}
		<-done
		return ctx.Err()
	}
}

func checkReady(config albums.GatewayConfig) error {
	host, port, err := net.SplitHostPort(config.ListenAddress)
	if err != nil {
		return err
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if address.IsUnspecified() {
		if address.Is6() {
			host = "::1"
		} else {
			host = "127.0.0.1"
		}
	}
	request, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/readyz", nil)
	if err != nil {
		return err
	}
	request.Host = config.APIHost
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 65))
	if err != nil || response.StatusCode != http.StatusOK || string(body) != `{"code":"ok"}` {
		return errors.New("not ready")
	}
	return nil
}
