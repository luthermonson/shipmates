package commands

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// initScaffold records the filesystem artifacts `shipmates init` creates so a
// failure part way through can leave the project exactly as it was found.
//
// Before this existed, an init that failed after scaffolding — the canonical
// case being a policy write lock that could not be acquired — left behind
// half-populated .shipmates/ and .codex/ trees. That is worse than a clean
// failure: the operator sees an initialized-looking project, and the next run
// takes the "already exists" branch for directories that were never finished.
//
// Only artifacts that did not exist beforehand are registered, so a rollback
// can never delete something the operator already had.
type initScaffold struct {
	created []string
}

// mkdirAll creates dir and any missing parents, registering for rollback only
// the components that did not already exist.
func (s *initScaffold) mkdirAll(dir string) error {
	cleaned := filepath.Clean(dir)
	if cleaned == "." || cleaned == string(filepath.Separator) {
		return nil
	}
	var cur string
	for _, part := range strings.Split(filepath.ToSlash(cleaned), "/") {
		if cur == "" {
			cur = part
		} else {
			cur = filepath.Join(cur, part)
		}
		if cur == "" {
			continue
		}
		_, err := os.Lstat(cur)
		if err == nil {
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Mkdir(cur, 0o755); err != nil {
			return err
		}
		s.created = append(s.created, cur)
	}
	return nil
}

// trackIfAbsent registers path for rollback when it does not exist yet. Call
// it immediately before the step that may create path.
func (s *initScaffold) trackIfAbsent(path string) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		s.created = append(s.created, path)
	}
}

// undo removes every artifact this scaffold created, deepest first, so a
// directory that init created is torn down with everything init put in it
// while a directory that predates init is left alone. Removal failures are
// logged rather than returned: the caller is already reporting the real
// error, and a stuck artifact must not mask it.
func (s *initScaffold) undo() {
	for i := len(s.created) - 1; i >= 0; i-- {
		if err := os.RemoveAll(s.created[i]); err != nil {
			slog.Warn("init rollback could not remove artifact", "path", s.created[i], "error", err)
			continue
		}
		slog.Debug("init rollback removed artifact", "path", s.created[i])
	}
	s.created = nil
}
