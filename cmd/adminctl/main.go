package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/tellyouwhat/backend/internal/adminauth"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if len(os.Args) != 2 || os.Args[1] != "bootstrap" {
		return errors.New("usage: adminctl bootstrap")
	}
	dsn := strings.TrimSpace(os.Getenv("DATABASE_DSN"))
	origin := strings.TrimRight(strings.TrimSpace(os.Getenv("ADMIN_ORIGIN")), "/")
	if dsn == "" || origin == "" {
		return errors.New("DATABASE_DSN and ADMIN_ORIGIN are required")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer database.Close()
	repository := adminauth.NewMySQLRepository(database)
	if _, exists, err := repository.Owner(context.Background()); err != nil {
		return err
	} else if exists {
		return errors.New("admin owner is already initialized")
	}
	token, err := adminauth.RandomToken(32)
	if err != nil {
		return err
	}
	expiresAt := time.Now().UTC().Add(15 * time.Minute)
	if err := repository.CreateBootstrapToken(context.Background(), adminauth.TokenHash(token), expiresAt); err != nil {
		return err
	}
	fmt.Printf("%s/setup#token=%s\nExpires: %s\n", origin, token, expiresAt.Format(time.RFC3339))
	return nil
}
