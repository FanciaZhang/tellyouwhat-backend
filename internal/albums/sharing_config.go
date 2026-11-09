package albums

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
)

type AlbumShareConfig struct {
	SiteOrigin string
	Keys       [][]byte
}

func (AlbumShareConfig) String() string   { return "[redacted album sharing configuration]" }
func (AlbumShareConfig) GoString() string { return "[redacted album sharing configuration]" }
func loadAlbumShareConfig(getenv func(string) string, excluded ...[]byte) (*AlbumShareConfig, error) {
	enabled := strings.TrimSpace(getenv("ALBUM_SHARING_ENABLED"))
	if enabled == "" || enabled == "false" {
		return nil, nil
	}
	if enabled != "true" {
		return nil, errors.New("invalid ALBUM_SHARING_ENABLED")
	}
	c := &AlbumShareConfig{SiteOrigin: strings.TrimSpace(getenv("ALBUM_SHARE_SITE_ORIGIN"))}
	if !validShareOrigin(c.SiteOrigin) {
		return nil, errors.New("invalid ALBUM_SHARE_SITE_ORIGIN")
	}
	for _, name := range []string{"ALBUM_SHARE_KEY_FILE", "ALBUM_SHARE_PREVIOUS_KEY_FILE"} {
		file := strings.TrimSpace(getenv(name))
		if name == "ALBUM_SHARE_PREVIOUS_KEY_FILE" && file == "" {
			continue
		}
		data, err := readAlbumSecretFile(file, 128)
		text := strings.TrimSpace(string(data))
		key, decodeErr := base64.StdEncoding.DecodeString(text)
		if err != nil || decodeErr != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != text {
			return nil, errors.New("invalid " + name)
		}
		for _, other := range append(excluded, c.Keys...) {
			if bytes.Equal(key, other) {
				return nil, errors.New("album share keys must differ from other secrets")
			}
		}
		c.Keys = append(c.Keys, key)
	}
	return c, nil
}
