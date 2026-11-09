package readaloud

import (
	"errors"
	"io"
	"os"
	"strings"
)

var ErrCredentials = errors.New("read-aloud credentials unavailable")

func privateFile(path string) ([]byte, error) {
	link, err := os.Lstat(path)
	if err != nil || !link.Mode().IsRegular() {
		return nil, ErrCredentials
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrCredentials
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(link, info) || !info.Mode().IsRegular() || info.Size() > 8192 || info.Mode().Perm()&0077 != 0 {
		return nil, ErrCredentials
	}
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(data) > 8192 {
		return nil, ErrCredentials
	}
	return data, nil
}

// No environment key fallback or configurable vendor endpoint is accepted.
func NewProviderFromFile(path string) (*Provider, error) {
	raw, err := privateFile(path)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(string(raw))
	if key == "" || len(key) > 4096 {
		return nil, ErrCredentials
	}
	for _, r := range key {
		if r < 33 || r > 126 {
			return nil, ErrCredentials
		}
	}
	return NewProvider(Endpoint, key, nil)
}
