package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return usageError()
	}
	dsn := strings.TrimSpace(os.Getenv("DATABASE_DSN"))
	if dsn == "" {
		return errors.New("DATABASE_DSN is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := mysqlstore.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer database.Close()
	repository := adminauth.NewMySQLRepository(database)
	switch os.Args[1] {
	case "bootstrap":
		if len(os.Args) != 2 {
			return usageError()
		}
		return bootstrap(ctx, repository)
	case "users":
		if len(os.Args) != 2 {
			return usageError()
		}
		return listUsers(ctx, repository)
	case "recover":
		if len(os.Args) != 3 {
			return usageError()
		}
		return recover(ctx, repository, strings.TrimSpace(os.Args[2]))
	default:
		return usageError()
	}
}

func bootstrap(ctx context.Context, repository *adminauth.MySQLRepository) error {
	origin, err := adminOrigin()
	if err != nil {
		return err
	}
	if initialized, err := repository.Initialized(ctx); err != nil {
		return err
	} else if initialized {
		return errors.New("admin service is already initialized")
	}
	token, err := adminauth.RandomToken(32)
	if err != nil {
		return err
	}
	expiresAt := time.Now().UTC().Add(15 * time.Minute)
	if err := repository.CreateBootstrapToken(ctx, adminauth.TokenHash(token), expiresAt); err != nil {
		return err
	}
	printEnrollmentURL(origin, "setup", token, expiresAt)
	return appendCLIAudit(ctx, repository, "admin.bootstrap_link.create", "", "")
}

func listUsers(ctx context.Context, repository *adminauth.MySQLRepository) error {
	users, err := repository.ListUsers(ctx)
	if err != nil {
		return err
	}
	for _, user := range users {
		fmt.Printf("%s\t%s\t%s\t%s\tpasskeys=%d\tapps=%s\n",
			user.ID, user.DisplayName, user.Role, user.Status, user.CredentialCount, strings.Join(user.AppIDs, ","))
	}
	return nil
}

func recover(ctx context.Context, repository *adminauth.MySQLRepository, userID string) error {
	if userID == "" {
		return usageError()
	}
	origin, err := adminOrigin()
	if err != nil {
		return err
	}
	user, found, err := repository.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !found || user.Status != adminauth.UserStatusActive {
		return errors.New("active management user was not found")
	}
	identifier, err := adminauth.NewUUID()
	if err != nil {
		return err
	}
	token, err := adminauth.RandomToken(32)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(15 * time.Minute)
	invitation := adminauth.Invitation{
		ID: identifier, Kind: adminauth.InvitationKindRecovery, TargetUserID: user.ID,
		DisplayName: user.DisplayName, Role: user.Role, AppIDs: user.AppIDs,
		CreatedAt: now, ExpiresAt: expiresAt,
	}
	if err := repository.CreateInvitation(ctx, adminauth.TokenHash(token), invitation); err != nil {
		return err
	}
	printEnrollmentURL(origin, "enroll", token, expiresAt)
	return appendCLIAudit(ctx, repository, "admin.passkey.recovery_invite", "admin_user", user.ID)
}

func adminOrigin() (string, error) {
	origin := strings.TrimRight(strings.TrimSpace(os.Getenv("ADMIN_ORIGIN")), "/")
	if origin == "" {
		return "", errors.New("ADMIN_ORIGIN is required")
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Port() != "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("ADMIN_ORIGIN must be a bare HTTPS origin")
	}
	return origin, nil
}

func appendCLIAudit(
	ctx context.Context,
	repository *adminauth.MySQLRepository,
	action string,
	targetType string,
	targetID string,
) error {
	requestID, err := adminauth.NewUUID()
	if err != nil {
		return fmt.Errorf("the one-time link above is valid, but its audit ID could not be generated: %w", err)
	}
	if err := repository.AppendAudit(ctx, adminauth.AuditEvent{
		RequestID: requestID, Action: action, TargetType: targetType, TargetID: targetID,
		Outcome: "succeeded", Metadata: map[string]any{"source": "adminctl"},
	}); err != nil {
		return fmt.Errorf("the one-time link above is valid, but its audit event could not be recorded: %w", err)
	}
	return nil
}

func printEnrollmentURL(origin, path, token string, expiresAt time.Time) {
	fmt.Printf("%s/%s#token=%s\nExpires: %s\n", origin, path, token, expiresAt.Format(time.RFC3339))
}

func usageError() error {
	return errors.New("usage: adminctl bootstrap | adminctl users | adminctl recover <user-id>")
}
