package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/tellyouwhat/backend/internal/media"
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
	store, err := media.NewTOSStore(media.TOSConfig{
		Endpoint: os.Getenv("TOS_ENDPOINT"), Region: os.Getenv("TOS_REGION"), Bucket: os.Getenv("TOS_BUCKET"),
		AccessKey: os.Getenv("TOS_ACCESS_KEY"), SecretKey: os.Getenv("TOS_SECRET_KEY"),
	})
	if err != nil {
		log.Fatal("object storage configuration is required for retention cleanup")
	}
	repository := mysqlstore.NewMaintenanceRepository(database)
	now := time.Now().UTC()
	purgedMedia, err := repository.PurgeExpiredMedia(ctx, now, store)
	if err != nil {
		log.Fatal(err)
	}
	result, err := repository.Cleanup(ctx, now)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf(
		"maintenance completed: idempotency=%d media=%d jobs=%d notifications=%d usage=%d identities=%d",
		result.IdempotencyRecords,
		result.MediaObjects+purgedMedia,
		result.Jobs,
		result.Notifications,
		result.UsageRecords,
		result.IdentityRecords,
	)
}
