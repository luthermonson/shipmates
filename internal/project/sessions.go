package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func SessionMarker(persona string) string {
	return filepath.Join(SessionsDir(), persona+".session")
}

func BackendSessionMarker(persona, backend string) string {
	backend = strings.TrimSpace(backend)
	if backend == "" || backend == "claude" {
		return SessionMarker(persona)
	}
	return filepath.Join(SessionsDir(), persona+"."+backend+".session")
}

type SessionMeta struct {
	Name       string `json:"name"`
	ID         string `json:"id"`
	ConfigHash string `json:"config"`
}

func ReadSessionMeta(persona string) (SessionMeta, bool) {
	return ReadBackendSessionMeta(persona, "claude")
}

func ReadBackendSessionMeta(persona, backend string) (meta SessionMeta, ok bool) {
	b, err := os.ReadFile(BackendSessionMarker(persona, backend))
	if err != nil {
		return SessionMeta{}, false
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return SessionMeta{Name: strings.TrimSpace(string(b))}, true
	}
	return meta, true
}

func WriteSessionMeta(persona, name, id, configHash string) error {
	return WriteBackendSessionMeta(persona, "claude", name, id, configHash)
}

func WriteBackendSessionMeta(persona, backend, name, id, configHash string) error {
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(SessionMeta{Name: name, ID: id, ConfigHash: configHash}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(BackendSessionMarker(persona, backend), b, 0o644)
}

func DeleteSessionMeta(persona string) error {
	return DeleteBackendSessionMeta(persona, "claude")
}

func DeleteBackendSessionMeta(persona, backend string) error {
	err := os.Remove(BackendSessionMarker(persona, backend))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}
