package main

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/tellyouwhat/backend/internal/promptconfig"
)

type journalPromptFile struct{ path string }

func (file journalPromptFile) Published(context.Context) (map[string]promptconfig.Revision, error) {
	info, err := os.Lstat(file.path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0007 != 0 || info.Size() > 512*1024 {
		return nil, promptconfig.ErrInvalid
	}
	input, err := os.Open(file.path)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	raw, err := io.ReadAll(io.LimitReader(input, 512*1024+1))
	if err != nil || len(raw) > 512*1024 {
		return nil, promptconfig.ErrInvalid
	}
	var revision promptconfig.Revision
	if err = json.Unmarshal(raw, &revision); err != nil {
		return nil, promptconfig.ErrInvalid
	}
	return map[string]promptconfig.Revision{"journal": revision}, nil
}
