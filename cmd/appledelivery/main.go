package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tellyouwhat/backend/internal/appledelivery"
)

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("select serve, publish or cleanup")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	data := flags.String("data", "", "private installation store")
	origin := flags.String("origin", "", "public HTTPS origin")
	listen := flags.String("listen", "127.0.0.1:18791", "HTTP listener behind the TLS proxy")
	candidate := flags.String("candidate", "", "directory containing verified candidate.json and development.ipa")
	receipt := flags.String("receipt", "", "private output file; installation capabilities are never printed")
	ttl := flags.Duration("ttl", appledelivery.MaxTTL, "installation link lifetime, at most 24h")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *data == "" || flags.NArg() != 0 {
		return fmt.Errorf("a private --data directory is required")
	}
	s, err := appledelivery.Open(*data)
	if err != nil {
		return err
	}
	defer s.Close()
	switch args[0] {
	case "publish":
		if *candidate == "" || *receipt == "" {
			return fmt.Errorf("publish requires --candidate and --receipt")
		}
		if _, err := os.Lstat(*receipt); !os.IsNotExist(err) {
			return fmt.Errorf("receipt output must not already exist")
		}
		parent, err := os.Stat(filepath.Dir(*receipt))
		if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("receipt directory must be private")
		}
		// Complete project-specific signing and release gates before invoking this trusted control plane.
		c, err := appledelivery.ReadCandidate(filepath.Join(*candidate, "candidate.json"))
		if err != nil {
			return err
		}
		ipa := filepath.Join(*candidate, "development.ipa")
		info, err := os.Lstat(ipa)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("candidate IPA must be a regular file")
		}
		f, err := os.Open(ipa)
		if err != nil {
			return err
		}
		defer f.Close()
		r, err := s.Publish(c, f, *origin, *ttl, time.Now())
		if err != nil {
			return err
		}
		payload, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		// Link commits a complete private receipt without overwriting an existing file.
		tmp, err := os.CreateTemp(filepath.Dir(*receipt), ".receipt-")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, err = tmp.Write(append(payload, '\n'))
		syncErr := tmp.Sync()
		closeErr := tmp.Close()
		if err != nil || syncErr != nil || closeErr != nil {
			return fmt.Errorf("cannot save private installation receipt")
		}
		if err := os.Link(tmp.Name(), *receipt); err != nil {
			return err
		}
		fmt.Println(`{"published":true,"receipt_saved":true}`)
		return nil
	case "cleanup":
		result, err := s.Cleanup(time.Now())
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "serve":
		h, err := appledelivery.NewHandler(s, *origin)
		if err != nil {
			return err
		}
		server := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second,
			IdleTimeout: 15 * time.Second, WriteTimeout: 15 * time.Minute, MaxHeaderBytes: 16 * 1024, ErrorLog: log.New(io.Discard, "", 0)}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		}()
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("installation listener failed")
		}
		return nil
	default:
		return fmt.Errorf("select serve, publish or cleanup")
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Apple delivery failed:", err)
		os.Exit(1)
	}
}
