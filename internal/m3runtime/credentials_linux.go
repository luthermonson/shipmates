//go:build linux

// Package m3runtime owns the unprivileged systemd credential boundary. It
// never opens the root-owned source credential paths.
package m3runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/luthermonson/shipmates/internal/fleetconfig"
	"github.com/luthermonson/shipmates/internal/fleettunnel"
	"github.com/luthermonson/shipmates/internal/installer"
	"golang.org/x/sys/unix"
)

const (
	ShipCredentialName      = "ship.json"
	CommanderCredentialName = "commander.json"
	CredentialsDirectoryEnv = "CREDENTIALS_DIRECTORY"
	maxCredentialBytes      = 16 << 10
)

type Loaded struct {
	Profile fleetconfig.Profile
	DirFD   int
}

// LoadFromEnvironment accepts only the manager-created credential directory
// and fixed basenames. It retains the directory and child FDs until Close.
func LoadFromEnvironment() (Loaded, error) {
	dir := os.Getenv(CredentialsDirectoryEnv)
	if !systemdCredentialDirectory(dir) {
		return Loaded{}, errors.New("credentials_directory_invalid")
	}
	return loadFromDirectory(dir, "/etc/shipmates/m3-qualifier/fleet.json", "/etc/shipmates/m3-qualifier/trust", true, true)
}

func LoadFromDirectory(dir, configPath, trustRoot string) (Loaded, error) {
	return loadFromDirectory(dir, configPath, trustRoot, true, false)
}

func loadFromDirectory(dir, configPath, trustRoot string, allowCurrentOwner, systemdRuntime bool) (Loaded, error) {
	if !safeAbsolute(dir) {
		return Loaded{}, errors.New("credentials_directory_invalid")
	}
	fd, err := openDirectory(dir, allowCurrentOwner)
	if err != nil {
		return Loaded{}, errors.New("credentials_directory_invalid")
	}
	if systemdRuntime && !managerOwnedRuntimeDirectory(fd) {
		unix.Close(fd)
		return Loaded{}, errors.New("credentials_directory_invalid")
	}
	files, err := os.ReadDir("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil || len(files) != 2 {
		unix.Close(fd)
		return Loaded{}, errors.New("credential_set_invalid")
	}
	want := map[string]bool{ShipCredentialName: false, CommanderCredentialName: false}
	for _, entry := range files {
		if _, ok := want[entry.Name()]; !ok {
			unix.Close(fd)
			return Loaded{}, errors.New("credential_set_invalid")
		}
		want[entry.Name()] = true
	}
	if !want[ShipCredentialName] || !want[CommanderCredentialName] {
		unix.Close(fd)
		return Loaded{}, errors.New("credential_set_incomplete")
	}
	shipFD, err := openCredential(fd, ShipCredentialName, allowCurrentOwner)
	if err != nil {
		unix.Close(fd)
		return Loaded{}, errors.New("ship_credential_invalid")
	}
	commanderFD, err := openCredential(fd, CommanderCredentialName, allowCurrentOwner)
	if err != nil {
		unix.Close(shipFD)
		unix.Close(fd)
		return Loaded{}, errors.New("commander_credential_invalid")
	}
	shipBytes, shipErr := readFD(shipFD)
	commanderBytes, commanderErr := readFD(commanderFD)
	if shipErr != nil || commanderErr != nil {
		clear(shipBytes)
		clear(commanderBytes)
		unix.Close(fd)
		return Loaded{}, errors.New("credential_read_failed")
	}
	profile, err := fleetconfig.LoadRuntimeProfileWithRecords(configPath, trustRoot, shipBytes, commanderBytes, allowCurrentOwner)
	clear(shipBytes)
	clear(commanderBytes)
	if err != nil {
		unix.Close(fd)
		return Loaded{}, err
	}
	return Loaded{Profile: profile, DirFD: fd}, nil
}

func systemdCredentialDirectory(path string) bool {
	return safeAbsolute(path) && filepath.Dir(path) == "/run/credentials" && validUnitLeaf(filepath.Base(path))
}

func validUnitLeaf(name string) bool {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (l *Loaded) Close() error {
	if l == nil || l.DirFD < 0 {
		return nil
	}
	err := unix.Close(l.DirFD)
	l.DirFD = -1
	return err
}

// Run validates the delivered records and then enters the existing real
// production TLS/M7/M3 submission seam. The runner itself supplies no target,
// credential, or endpoint override.
func Run(ctx context.Context) error {
	// The lifecycle lock is acquired before reading CREDENTIALS_DIRECTORY, so a
	// concurrent activation cannot observe or use a credential record first.
	lease, err := installer.AcquireQualifierLifecycleLease("/", false)
	if err != nil {
		return err
	}
	defer lease.Close()
	if err := lease.Recheck(); err != nil {
		return err
	}
	loaded, err := LoadFromEnvironment()
	if err != nil {
		return err
	}
	defer loaded.Close()
	return fleettunnel.QualifyProfile(ctx, loaded.Profile)
}

func ValidateInvocation(args []string, env []string) error {
	if len(args) != 0 {
		return errors.New("runner_arguments_refused")
	}
	seen := false
	for _, value := range env {
		key, raw, ok := strings.Cut(value, "=")
		if key == CredentialsDirectoryEnv {
			if seen || !systemdCredentialDirectory(raw) {
				return errors.New("credentials_directory_invalid")
			}
			seen = true
			continue
		}
		if ok && (strings.HasPrefix(key, "SHIPMATES_") || strings.Contains(strings.ToUpper(key), "SECRET") || strings.Contains(strings.ToUpper(key), "TOKEN")) {
			return errors.New("runner_override_refused")
		}
	}
	if !seen {
		return errors.New("credentials_directory_required")
	}
	return nil
}

func safeAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.Contains(path, "//")
}

func openDirectory(path string, allowCurrentOwner bool) (int, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0022 != 0 || st.Nlink < 2 || (st.Uid != 0 && (!allowCurrentOwner || int(st.Uid) != os.Getuid())) {
		unix.Close(fd)
		return -1, errors.New("unsafe credential directory")
	}
	return fd, nil
}

func managerOwnedRuntimeDirectory(fd int) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR && int(st.Uid) == os.Getuid() && st.Mode&0077 == 0
}

func openCredential(dirFD int, name string, allowCurrentOwner bool) (int, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0077 != 0 || (st.Uid != 0 && (!allowCurrentOwner || int(st.Uid) != os.Getuid())) {
		unix.Close(fd)
		return -1, errors.New("unsafe credential file")
	}
	return fd, nil
}

func readFD(fd int) ([]byte, error) {
	f := os.NewFile(uintptr(fd), "credential")
	if f == nil {
		return nil, errors.New("credential_fd_invalid")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	if err != nil || len(b) > maxCredentialBytes {
		return nil, errors.New("credential_too_large")
	}
	return b, nil
}

func clear(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
