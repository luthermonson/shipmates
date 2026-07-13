//go:build windows

package project

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type windowsFileIdentity struct {
	volume uint64
	index  uint64
	size   uint64
	write  windows.Filetime
	links  uint32
}

func openWindowsInventoryHandle(path string, directory bool) (windows.Handle, windowsFileIdentity, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, windowsFileIdentity{}, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return windows.InvalidHandle, windowsFileIdentity{}, err
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return windows.InvalidHandle, windowsFileIdentity{}, err
	}
	isDir := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || isDir != directory {
		windows.CloseHandle(h)
		return windows.InvalidHandle, windowsFileIdentity{}, errors.New("unsafe Windows filesystem object")
	}
	id := windowsFileIdentity{
		volume: uint64(info.VolumeSerialNumber) | 1<<63,
		index:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow) | 1<<63,
		size:   uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow),
		write:  info.LastWriteTime,
		links:  info.NumberOfLinks,
	}
	return h, id, nil
}

func sameWindowsIdentity(a, b windowsFileIdentity) bool {
	return a.volume == b.volume && a.index == b.index && a.size == b.size && a.write == b.write && a.links == b.links
}

func readCanonicalPersonaInventory(root, wanted string) ([]InstalledPersona, error) {
	rootHandle, _, err := openWindowsInventoryHandle(root, true)
	if err != nil {
		return nil, fmt.Errorf("unsafe Codex persona inventory root")
	}
	defer windows.CloseHandle(rootHandle)

	codexPath := filepath.Join(root, ".codex")
	codexHandle, _, err := openWindowsInventoryHandle(codexPath, true)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("unsafe Codex persona inventory directory")
	}
	defer windows.CloseHandle(codexHandle)

	agentsPath := filepath.Join(codexPath, "agents")
	agentsHandle, agentsIdentity, err := openWindowsInventoryHandle(agentsPath, true)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("unsafe Codex persona inventory directory")
	}
	agentsFile := os.NewFile(uintptr(agentsHandle), agentsPath)
	defer agentsFile.Close()
	entries, err := agentsFile.ReadDir(MaxInstalledPersonas + 1)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read Codex persona inventory: %w", err)
	}
	if len(entries) > MaxInstalledPersonas {
		return nil, fmt.Errorf("Codex persona inventory exceeds limit of %d", MaxInstalledPersonas)
	}
	out := make([]InstalledPersona, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".toml" || strings.TrimSuffix(name, ".toml") == "" {
			return nil, fmt.Errorf("unsafe entry in Codex persona inventory")
		}
		persona := strings.TrimSuffix(name, ".toml")
		if wanted != "" && persona != wanted {
			continue
		}
		h, before, err := openWindowsInventoryHandle(filepath.Join(agentsPath, name), false)
		if err != nil || before.links != 1 || before.size > MaxCodexAgentBytes {
			if h != windows.InvalidHandle {
				windows.CloseHandle(h)
			}
			return nil, fmt.Errorf("persona %q has an unsafe Codex artifact", persona)
		}
		f := os.NewFile(uintptr(h), name)
		raw, readErr := io.ReadAll(io.LimitReader(f, MaxCodexAgentBytes+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || len(raw) > MaxCodexAgentBytes {
			return nil, fmt.Errorf("persona %q has an unsafe Codex artifact", persona)
		}
		h2, after, reopenErr := openWindowsInventoryHandle(filepath.Join(agentsPath, name), false)
		if reopenErr == nil {
			windows.CloseHandle(h2)
		}
		if reopenErr != nil || !sameWindowsIdentity(before, after) {
			return nil, fmt.Errorf("persona %q changed during inventory read", persona)
		}
		out = append(out, InstalledPersona{Name: persona, Raw: raw, Mode: 0o600, Device: before.volume, Inode: before.index})
	}
	check, afterAgents, err := openWindowsInventoryHandle(agentsPath, true)
	if err == nil {
		windows.CloseHandle(check)
	}
	if err != nil || !sameWindowsIdentity(agentsIdentity, afterAgents) {
		return nil, fmt.Errorf("Codex persona inventory directory changed during read")
	}
	return out, nil
}
