package albums

import (
	"errors"
	"strings"
)

func loadRetentionAPNSConfig(getenv func(string) string, gateway GatewayConfig) (*albumAPNSConfig, error) {
	enabled := strings.TrimSpace(getenv("ALBUM_RETENTION_PUSH_ENABLED"))
	if enabled == "" || enabled == "false" {
		return nil, nil
	}
	if enabled != "true" || gateway.Subscriptions == nil || gateway.PushEnvironment == "" {
		return nil, errors.New("album retention push requires subscriptions and registration")
	}
	c := &albumAPNSConfig{BundleID: gateway.AppleCode.BundleID, TeamID: gateway.AppleCode.TeamID, KeyID: strings.TrimSpace(getenv("ALBUM_APNS_KEY_ID")), Environment: gateway.PushEnvironment}
	if c.KeyID == gateway.AppleCode.KeyID || c.KeyID == gateway.Subscriptions.KeyID {
		return nil, errors.New("album APNs requires a separate signing key")
	}
	key, err := readAlbumSecretFile(getenv("ALBUM_APNS_KEY_FILE"), 16<<10)
	if err != nil {
		return nil, errors.New("invalid ALBUM_APNS_KEY_FILE")
	}
	c.PrivateKeyPEM = key
	if _, err := newAlbumAPNS(*c, nil); err != nil {
		return nil, err
	}
	return c, nil
}
