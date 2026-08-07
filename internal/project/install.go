package project

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func RepoName() string {
	wd, err := os.Getwd()
	if err != nil {
		return "shipmates"
	}
	return filepath.Base(wd)
}

func InstallID() (string, error) {
	b, err := os.ReadFile(InstallIDPath())
	if err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	id := NewUUID()
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(InstallIDPath(), []byte(id+"\n"), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
