package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func main() {
	databaseDSN := os.Getenv("DATABASE_DSN")
	if databaseDSN == "" {
		log.Fatal("DATABASE_DSN is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	database, err := mysqlstore.Open(ctx, databaseDSN)
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()
	result, err := mysqlstore.NewMaintenanceRepository(database).Cleanup(ctx, time.Now().UTC())
	if err != nil {
		log.Fatal(err)
	}
	log.Printf(
		"maintenance completed: idempotency=%d media=%d jobs=%d notifications=%d usage=%d identities=%d",
		result.IdempotencyRecords,
		result.MediaObjects,
		result.Jobs,
		result.Notifications,
		result.UsageRecords,
		result.IdentityRecords,
	)
}
