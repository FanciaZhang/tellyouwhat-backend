// quotarepair previews or converts legacy Health job prepayments into uncharged
// execution bindings. Output contains aggregate accounting metadata only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/storage/redisstore"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "quota repair failed; no credentials or request contents are logged")
		os.Exit(1)
	}
}
func run() error {
	apply := flag.Bool("apply", false, "convert unclaimed reservations after upgrading all gateway and worker instances")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := mysqlstore.Open(ctx, os.Getenv("DATABASE_DSN"))
	if err != nil {
		return err
	}
	defer db.Close()
	client, err := redisstore.Open(ctx, os.Getenv("REDIS_URL"))
	if err != nil {
		return err
	}
	defer client.Close()
	// The job-capability namespace differs from synchronous request reservations.
	// Recompute keys from durable request bindings rather than scanning accounts.
	rows, err := db.QueryContext(ctx, `SELECT owner_key_id,request_id,body_digest FROM idempotency_records WHERE app_id='health' AND created_at > UTC_TIMESTAMP()-INTERVAL 26 HOUR ORDER BY created_at LIMIT 10001`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var key, request, digest string
		if err := rows.Scan(&key, &request, &digest); err != nil {
			return err
		}
		ids = append(ids, quota.JobReservationID(key, request, digest))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) > 10000 {
		return fmt.Errorf("repair batch exceeds limit")
	}
	limiter := redisstore.NewQuotaLimiter(client, quota.Limits{}, "health")
	count, tokens := 0, 0
	for _, id := range ids {
		n, err := limiter.DeferLegacyJob(ctx, id, *apply)
		if err != nil {
			return err
		}
		if n > 0 {
			count++
			tokens += n
		}
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Apply        bool `json:"apply"`
		Reservations int  `json:"reservations"`
		Tokens       int  `json:"tokens"`
	}{*apply, count, tokens})
}
