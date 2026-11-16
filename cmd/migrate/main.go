package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

func main() {
	// The flag name is stored in immutable registered Compose selections.
	albumMetadata := flag.Bool("album-development-products", false, "apply only reviewed Albums development metadata migrations")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("unexpected migration arguments")
	}
	databaseDSN := os.Getenv("DATABASE_DSN")
	if databaseDSN == "" {
		log.Fatal("DATABASE_DSN is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	database, err := mysqlstore.Open(ctx, databaseDSN)
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()
	run := migrations.Run
	if *albumMetadata {
		run = migrations.RunAlbumDevelopmentMetadata
	}
	if err := run(ctx, database); err != nil {
		log.Fatal(err)
	}
}
