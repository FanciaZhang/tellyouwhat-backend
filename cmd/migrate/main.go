package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

func main() {
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
	if err := migrations.Run(ctx, database); err != nil {
		log.Fatal(err)
	}
}
