package albums

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

// GatewayConfig is independent of Health/Journal/TOS and has no debug owner,
// identity-cipher, or free-quota fallback. Secret files are server-only.
type GatewayConfig struct {
	Worker          WorkerConfig
	Environment     string
	APIHost         string
	ListenAddress   string
	RedisURL        string
	TrustedProxies  []string
	AppleCode       AppleCodeConfig
	CredentialKey   string
	LoginLimitKey   []byte
	Subscriptions   *AlbumSubscriptionConfig
	Sharing         *AlbumShareConfig
	PushEnvironment string
	RetentionAPNS   *albumAPNSConfig
}

func (GatewayConfig) String() string   { return "[redacted album gateway configuration]" }
func (GatewayConfig) GoString() string { return "[redacted album gateway configuration]" }
func (GatewayConfig) LogValue() slog.Value {
	return slog.StringValue("[redacted album gateway configuration]")
}

var albumTestDatabasePattern = regexp.MustCompile(`^albums(?:_[a-z0-9]+)*_test$`)
var albumAPIHostPattern = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,63}$`)

func LoadGatewayConfig(getenv func(string) string) (GatewayConfig, error) {
	worker, err := LoadWorkerConfig(getenv)
	if err != nil {
		return GatewayConfig{}, err
	}
	c := GatewayConfig{Worker: worker, Environment: strings.TrimSpace(getenv("ALBUM_DEPLOYMENT_ENVIRONMENT")),
		APIHost: strings.TrimSpace(getenv("ALBUM_API_HOST")), ListenAddress: strings.TrimSpace(getenv("ALBUM_HTTP_LISTEN_ADDRESS")),
		RedisURL: strings.TrimSpace(getenv("ALBUM_REDIS_URL"))}
	if c.ListenAddress == "" {
		c.ListenAddress = "127.0.0.1:8080"
	}
	db, err := mysql.ParseDSN(c.Worker.DatabaseDSN)
	if err != nil || db.User == "" || strings.EqualFold(db.User, "root") || (c.Environment != "test" && c.Environment != "production") ||
		(c.Environment == "test" && !albumTestDatabasePattern.MatchString(db.DBName)) ||
		(c.Environment == "production" && db.DBName != "albums" && db.DBName != "albums_production") {
		return GatewayConfig{}, errors.New("invalid independent album database/environment")
	}
	if !albumAPIHostPattern.MatchString(c.APIHost) || len(c.APIHost) > 253 {
		return GatewayConfig{}, errors.New("invalid ALBUM_API_HOST")
	}
	host, port, err := net.SplitHostPort(c.ListenAddress)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 1 || number > 65535 {
		return GatewayConfig{}, errors.New("invalid ALBUM_HTTP_LISTEN_ADDRESS")
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return GatewayConfig{}, errors.New("invalid ALBUM_HTTP_LISTEN_ADDRESS")
	}
	options, err := redis.ParseURL(c.RedisURL)
	if err != nil || options.Addr == "" || options.DB != 0 {
		return GatewayConfig{}, errors.New("invalid dedicated ALBUM_REDIS_URL")
	}
	// Redis DB numbers alone are not instance isolation; Compose uses a dedicated
	// Redis container. Trust only the explicitly selected private proxy network.
	if raw := strings.TrimSpace(getenv("ALBUM_TRUSTED_PROXIES")); raw != "" {
		for _, item := range strings.Split(raw, ",") {
			p, err := netip.ParsePrefix(strings.TrimSpace(item))
			if err != nil || p != p.Masked() || (!p.Addr().IsPrivate() && !p.Addr().IsLoopback()) || p.Bits() == 0 {
				return GatewayConfig{}, errors.New("invalid ALBUM_TRUSTED_PROXIES")
			}
			c.TrustedProxies = append(c.TrustedProxies, p.String())
		}
	}
	c.AppleCode.TeamID = strings.TrimSpace(getenv("ALBUM_APPLE_TEAM_ID"))
	c.AppleCode.KeyID = strings.TrimSpace(getenv("ALBUM_APPLE_SIGN_IN_KEY_ID"))
	c.AppleCode.PrivateKeyPEM, err = readAlbumSecretFile(getenv("ALBUM_APPLE_SIGN_IN_KEY_FILE"), 16<<10)
	if err != nil {
		return GatewayConfig{}, errors.New("invalid ALBUM_APPLE_SIGN_IN_KEY_FILE")
	}
	if _, err := NewAppleCodeExchanger(c.AppleCode, NewAppleIdentityVerifier(nil), nil); err != nil {
		return GatewayConfig{}, errors.New("invalid album Apple Sign in configuration")
	}
	keys := make([][]byte, 0, 2)
	for _, name := range []string{"ALBUM_CREDENTIAL_KEY_FILE", "ALBUM_LOGIN_LIMIT_KEY_FILE"} {
		encoded, err := readAlbumSecretFile(getenv(name), 128)
		if err != nil {
			return GatewayConfig{}, errors.New("invalid " + name)
		}
		text := strings.TrimSpace(string(encoded))
		key, err := base64.StdEncoding.DecodeString(text)
		if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != text {
			return GatewayConfig{}, errors.New("invalid " + name)
		}
		keys = append(keys, key)
	}
	if bytes.Equal(keys[0], keys[1]) {
		return GatewayConfig{}, errors.New("album encryption and login privacy keys must differ")
	}
	c.CredentialKey = base64.StdEncoding.EncodeToString(keys[0])
	c.LoginLimitKey = keys[1]
	c.Sharing, err = loadAlbumShareConfig(getenv, keys...)
	if err != nil {
		return GatewayConfig{}, err
	}
	c.Subscriptions, err = loadAlbumSubscriptionConfig(getenv, c.Environment)
	if err != nil {
		return GatewayConfig{}, err
	}
	c.PushEnvironment = strings.TrimSpace(getenv("ALBUM_PUSH_ENVIRONMENT"))
	if c.PushEnvironment != "" && c.PushEnvironment != "development" && c.PushEnvironment != "production" {
		return GatewayConfig{}, errors.New("invalid ALBUM_PUSH_ENVIRONMENT")
	}
	c.RetentionAPNS, err = loadRetentionAPNSConfig(getenv, c)
	if err != nil {
		return GatewayConfig{}, err
	}
	return c, nil
}

func readAlbumSecretFile(path string, max int64) ([]byte, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("secret file unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("secret file unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	// 0600/0400 and owner+group-read 0440 are accepted for container mounts.
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > max || info.Mode().Perm()&0137 != 0 {
		return nil, errors.New("secret file unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || len(data) == 0 || int64(len(data)) > max {
		return nil, errors.New("secret file unavailable")
	}
	return data, nil
}
