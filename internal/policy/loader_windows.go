//go:build windows

// Windows policy snapshot capture.
//
// This file is the Windows twin of loader_unix.go and is held to the same
// security contract. The mapping from unix primitive to Windows primitive is:
//
//	openat + O_NOFOLLOW + O_DIRECTORY
//	    CreateFile with FILE_FLAG_OPEN_REPARSE_POINT (+ BACKUP_SEMANTICS for
//	    directories) and outright refusal when FILE_ATTRIBUTE_REPARSE_POINT
//	    comes back set, walked one component at a time by winsec.DirChain.
//	    Windows has no openat; the "resolve relative to a descriptor I have
//	    already validated" property is recovered by holding every ancestor
//	    handle open *without* FILE_SHARE_DELETE, which makes a verified
//	    ancestor un-renameable and un-deletable and therefore un-swappable,
//	    and by requiring GetFinalPathNameByHandle on the result to equal the
//	    path that was asked for.
//
//	flock(LOCK_SH) on the .shipmates directory descriptor
//	    LockFileEx (shared, blocking) on .shipmates\.policy.lock. Windows
//	    cannot lock a directory handle, so the one piece of state this
//	    platform must create is a zero-length lock object inside the policy
//	    directory. It is hardened to the current user + LOCAL SYSTEM and is
//	    never read or written, only locked. The exclusive side lives in
//	    internal/project/policylock_windows.go and must use the same name.
//
//	fstat/fstatat before and after, comparing dev+ino+size+mtim+ctim
//	    GetFileInformationByHandle (VolumeSerialNumber + FileIndex + size +
//	    NumberOfLinks + attributes + LastWriteTime + CreationTime),
//	    GetFileInformationByHandleEx(FileBasicInfo) for ChangeTime — the
//	    st_ctim analogue — and FileIdInfo for the ReFS-safe 128-bit id. See
//	    winsec.Identity.
//
//	0600 on files the loader creates
//	    a protected DACL granting only the process user and LOCAL SYSTEM,
//	    written and read back by winsec.PrivateDACL.
//
// One deliberate difference from unix: because handles are held without
// FILE_SHARE_DELETE, an attempt to rename or delete a policy file that the
// loader currently holds open does not race the loader — the operating system
// refuses it with ERROR_SHARING_VIOLATION. That is strictly stronger than the
// unix behaviour of allowing the swap and detecting it afterwards, and the
// after-the-fact detection below is retained anyway.
package policy

import (
	"errors"
	"path/filepath"

	"github.com/luthermonson/shipmates/internal/winsec"
	"golang.org/x/sys/windows"
)

// policyLockName is the shared policy synchronization object inside
// .shipmates. Keep it identical to the name used by
// internal/project/policylock_windows.go: readers take it shared, policy
// mutations take it exclusive, and a mismatch would silently remove all
// cross-process exclusion.
const policyLockName = ".policy.lock"

type policyPath struct {
	desc       SourceDescriptor
	components []string
	optional   bool
}

type capturedPolicy struct {
	bytes  []byte
	absent bool
	// dir pins every directory between .shipmates and the leaf, so the
	// ancestors of a captured source cannot be replaced before revalidation.
	dir    *winsec.DirChain
	path   string
	handle windows.Handle
	id     winsec.Identity
}

func (c *capturedPolicy) close() {
	if c.handle != windows.InvalidHandle {
		windows.CloseHandle(c.handle)
		c.handle = windows.InvalidHandle
	}
	c.dir.Close()
}

// revalidate re-establishes, after every layer has been captured, that the
// name still denotes the captured object (or that a captured absence is still
// an absence). It is the Windows twin of the unix fstat + fstatat pair.
func (c *capturedPolicy) revalidate() bool {
	if c.absent {
		h, _, err := winsec.Open(c.path, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING)
		if err == nil {
			windows.CloseHandle(h)
			return false
		}
		return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
	}
	current, err := winsec.Identify(c.handle)
	if err != nil || current.IsDir() || current.IsReparsePoint() || current.Links != 1 || current != c.id {
		return false
	}
	named, id, err := winsec.Open(c.path, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING)
	if err != nil {
		return false
	}
	ok := winsec.VerifyFinalPath(named, c.path) == nil && id == current
	windows.CloseHandle(named)
	return ok
}

// testPolicyReadHook is a deterministic test barrier. Production leaves it nil.
var testPolicyReadHook func(int)

// testPolicyCaptureHook runs after the policy directory is locked and after
// the named layer has been captured. Production leaves it nil.
var testPolicyCaptureHook func(int)

func load(projectRoot, persona, projectRootID string) (*Snapshot, []Diagnostic) {
	// Validate the untrusted fragment before constructing any filesystem path.
	if !ruleIDRE.MatchString(persona) || len(persona) > 80 {
		return nil, loadDiagnostic("policy_outside_project", SourceDescriptor{Layer: LayerPersona, Path: ".shipmates/policies/<persona>.yaml"}, "persona is not a canonical policy path component")
	}
	if !filepath.IsAbs(projectRoot) || filepath.Clean(projectRoot) != projectRoot {
		return nil, loadDiagnostic("policy_outside_project", SourceDescriptor{Layer: LayerProject, Path: ".shipmates/policy.yaml"}, "project root is not canonical")
	}
	projectDesc := SourceDescriptor{Layer: LayerProject, Path: ".shipmates/policy.yaml"}

	root, err := winsec.OpenDirChain(projectRoot)
	if err != nil {
		return nil, loadDiagnostic(classifyOpen(projectRoot, err), projectDesc, "project root cannot be opened safely")
	}
	defer root.Close()
	// root.Path is the canonical form derived from the pinned handle, so this
	// second walk cannot be redirected: the root it starts from is the object
	// already held open above.
	shipmates, err := winsec.OpenDirChain(root.Path, ".shipmates")
	if err != nil {
		child, _ := winsec.Child(root.Path, ".shipmates")
		return nil, loadDiagnostic(classifyOpen(child, err), projectDesc, "policy directory cannot be opened safely")
	}
	defer shipmates.Close()

	// The lock object is the project-local synchronization point. Shipmates
	// policy mutations take it exclusively; holding it shared across all three
	// reads gives the snapshot one atomic capture interval without changing
	// authority.
	lock, err := winsec.OpenLockFile(shipmates.Path, policyLockName)
	if err != nil {
		return nil, loadDiagnostic("policy_unreadable", projectDesc, "policy directory cannot be locked safely")
	}
	defer windows.CloseHandle(lock)
	if err := winsec.Lock(lock, false, true); err != nil {
		return nil, loadDiagnostic("policy_unreadable", projectDesc, "policy directory cannot be locked safely")
	}
	// Released on every exit path including a panic, because the deferred
	// unlock runs during unwinding and the deferred CloseHandle above would
	// release the range regardless.
	defer winsec.Unlock(lock)

	paths := []policyPath{
		{SourceDescriptor{Layer: LayerProject, Path: ".shipmates/policy.yaml", Present: true}, []string{"policy.yaml"}, false},
		{SourceDescriptor{Layer: LayerProjectLocal, Path: ".shipmates/policy.local.yaml", Present: true}, []string{"policy.local.yaml"}, true},
		{SourceDescriptor{Layer: LayerPersona, Path: ".shipmates/policies/" + persona + ".yaml", Present: true}, []string{"policies", persona + ".yaml"}, false},
	}
	sources := make([]Source, 0, len(paths))
	captured := make([]*capturedPolicy, 0, len(paths))
	defer func() {
		for _, c := range captured {
			c.close()
		}
	}()
	for i, p := range paths {
		c, code := readPolicyIn(shipmates, p.components, p.optional)
		if code != "" {
			return nil, loadDiagnostic(code, p.desc, "policy source cannot be loaded safely")
		}
		captured = append(captured, c)
		p.desc.Present = !c.absent
		sources = append(sources, Source{Descriptor: p.desc, Bytes: c.bytes})
		if testPolicyCaptureHook != nil {
			testPolicyCaptureHook(i)
		}
	}
	// A snapshot is meaningful only if every name still denotes the captured
	// descriptor (and every captured absence is still absent) after all layers
	// have been collected. This prevents a mix of versions that never coexisted.
	for i, c := range captured {
		if !c.revalidate() {
			return nil, loadDiagnostic("policy_changed_during_load", paths[i].desc, "policy source changed while the snapshot was assembled")
		}
	}
	return Parse(persona, projectRootID, sources)
}

func readPolicyIn(shipmates *winsec.DirChain, components []string, optional bool) (*capturedPolicy, string) {
	// Re-walk from the pinned .shipmates handle. With zero intermediate
	// components this simply re-opens .shipmates, which keeps one code path
	// for every layer and gives each capture its own pinned ancestry.
	intermediate := components[:len(components)-1]
	dir, err := winsec.OpenDirChain(shipmates.Path, intermediate...)
	if err != nil {
		// Classify against the deepest component that could have failed, so a
		// probe of the name reports on the right object.
		blamed := shipmates.Path
		if len(intermediate) > 0 {
			if child, childErr := winsec.Child(shipmates.Path, intermediate[0]); childErr == nil {
				blamed = child
			}
		}
		return nil, classifyOpen(blamed, err)
	}
	leafPath, err := winsec.Child(dir.Path, components[len(components)-1])
	if err != nil {
		dir.Close()
		return nil, "policy_outside_project"
	}
	handle, before, err := winsec.Open(leafPath, false, windows.GENERIC_READ, windows.OPEN_EXISTING)
	if err != nil {
		if optional && (errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)) {
			return &capturedPolicy{absent: true, dir: dir, path: leafPath, handle: windows.InvalidHandle}, ""
		}
		code := classifyOpen(leafPath, err)
		dir.Close()
		return nil, code
	}
	fail := func(code string) (*capturedPolicy, string) {
		windows.CloseHandle(handle)
		dir.Close()
		return nil, code
	}
	if err := winsec.VerifyFinalPath(handle, leafPath); err != nil {
		return fail("policy_changed_during_load")
	}
	if before.Size > MaxSourceBytes {
		return fail("policy_too_large")
	}
	if testPolicyReadHook != nil {
		testPolicyReadHook(int(handle))
	}
	b, code := readAll(handle, before.Size)
	if code != "" {
		return fail(code)
	}
	after, err := winsec.Identify(handle)
	if err != nil {
		return fail("policy_unreadable")
	}
	if after != before || uint64(len(b)) != after.Size {
		return fail("policy_changed_during_load")
	}
	// The name must still denote this exact object: the analogue of the unix
	// fstatat(parent, leaf, AT_SYMLINK_NOFOLLOW) identity check.
	named, namedID, err := winsec.Open(leafPath, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING)
	if err != nil {
		return fail("policy_changed_during_load")
	}
	sameName := winsec.VerifyFinalPath(named, leafPath) == nil && namedID == after
	windows.CloseHandle(named)
	if !sameName {
		return fail("policy_changed_during_load")
	}
	return &capturedPolicy{bytes: b, dir: dir, path: leafPath, handle: handle, id: after}, ""
}

// readAll drains the handle from its current (zero) offset with a bounded
// buffer, refusing anything that grows past MaxSourceBytes mid-read.
func readAll(h windows.Handle, size uint64) ([]byte, string) {
	out := make([]byte, 0, size)
	buf := make([]byte, 32<<10)
	for {
		var n uint32
		if err := windows.ReadFile(h, buf, &n, nil); err != nil {
			if errors.Is(err, windows.ERROR_HANDLE_EOF) {
				break
			}
			return nil, "policy_unreadable"
		}
		if n == 0 {
			break
		}
		if uint64(len(out))+uint64(n) > MaxSourceBytes {
			return nil, "policy_too_large"
		}
		out = append(out, buf[:n]...)
	}
	return out, ""
}

// classifyOpen turns a failed safe open into a policy diagnostic code. Where
// the failure is ambiguous it probes the name the way unix classifyAtError
// uses fstatat: CreateFile refuses a directory opened as a file with a bare
// ERROR_ACCESS_DENIED, so without the probe "policy.yaml is a directory" and
// "policy.yaml is unreadable" would be indistinguishable.
func classifyOpen(path string, err error) string {
	switch {
	case errors.Is(err, winsec.ErrReparsePoint):
		return "policy_symlink"
	case errors.Is(err, winsec.ErrWrongType), errors.Is(err, winsec.ErrHardLink):
		return "policy_not_regular"
	case errors.Is(err, winsec.ErrPathMismatch):
		return "policy_changed_during_load"
	case errors.Is(err, winsec.ErrBadComponent):
		return "policy_outside_project"
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return "policy_missing"
	}
	if id, probeErr := winsec.Probe(path); probeErr == nil {
		switch {
		case id.IsReparsePoint():
			return "policy_symlink"
		case id.IsDir(), id.Links != 1:
			return "policy_not_regular"
		}
	} else if errors.Is(probeErr, windows.ERROR_FILE_NOT_FOUND) || errors.Is(probeErr, windows.ERROR_PATH_NOT_FOUND) {
		return "policy_missing"
	}
	return "policy_unreadable"
}
