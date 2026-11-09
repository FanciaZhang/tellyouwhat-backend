package mysqlstore

import (
	"context"
	"database/sql"
	"time"

	"github.com/go-sql-driver/mysql"
)

func Open(ctx context.Context, databaseDSN string) (*sql.DB, error) {
	config, err := mysql.ParseDSN(databaseDSN)
	if err != nil {
		return nil, err
	}
	config.ParseTime = true
	config.Loc = time.UTC
	config.MultiStatements = true
	database, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(10)
	database.SetMaxIdleConns(5)
	database.SetConnMaxLifetime(5 * time.Minute)
	database.SetConnMaxIdleTime(2 * time.Minute)
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

func affectedRows(result sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
