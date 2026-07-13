//go:build unix

package policy

import (
	"errors"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type policyPath struct {
	desc       SourceDescriptor
	components []string
	optional   bool
}

type capturedPolicy struct {
	bytes  []byte
	absent bool
	parent int
	leaf   string
	fd     int
	stat   unix.Stat_t
}

func (c *capturedPolicy) close() {
	if c.fd >= 0 {
		_ = unix.Close(c.fd)
	}
	if c.parent >= 0 {
		_ = unix.Close(c.parent)
	}
}

func (c *capturedPolicy) revalidate() bool {
	var named unix.Stat_t
	if c.absent {
		return errors.Is(unix.Fstatat(c.parent, c.leaf, &named, unix.AT_SYMLINK_NOFOLLOW), syscall.ENOENT)
	}
	var current unix.Stat_t
	if unix.Fstat(c.fd, &current) != nil || current.Mode&unix.S_IFMT != unix.S_IFREG ||
		current.Dev != c.stat.Dev || current.Ino != c.stat.Ino || current.Size != c.stat.Size ||
		current.Mtim != c.stat.Mtim || current.Ctim != c.stat.Ctim {
		return false
	}
	return unix.Fstatat(c.parent, c.leaf, &named, unix.AT_SYMLINK_NOFOLLOW) == nil &&
		named.Mode&unix.S_IFMT == unix.S_IFREG && named.Dev == current.Dev && named.Ino == current.Ino &&
		named.Size == current.Size && named.Mtim == current.Mtim && named.Ctim == current.Ctim
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
	root, err := unix.Open(projectRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, loadDiagnostic(classifyOpenError(err, false), SourceDescriptor{Layer: LayerProject, Path: ".shipmates/policy.yaml"}, "project root cannot be opened safely")
	}
	defer unix.Close(root)
	shipmates, err := unix.Openat(root, ".shipmates", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, loadDiagnostic(classifyAtError(root, ".shipmates", err, false), SourceDescriptor{Layer: LayerProject, Path: ".shipmates/policy.yaml"}, "policy directory cannot be opened safely")
	}
	defer unix.Close(shipmates)
	// This directory descriptor is the project-local synchronization object.
	// Shipmates policy mutations take LOCK_EX on the same safely opened
	// directory; holding LOCK_SH across all three reads gives the snapshot one
	// atomic capture interval without creating state or changing authority.
	if err := unix.Flock(shipmates, unix.LOCK_SH); err != nil {
		return nil, loadDiagnostic("policy_unreadable", SourceDescriptor{Layer: LayerProject, Path: ".shipmates/policy.yaml"}, "policy directory cannot be locked safely")
	}
	defer unix.Flock(shipmates, unix.LOCK_UN) // best-effort; close also releases it

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
		c, code := readPolicyAt(shipmates, p.components, p.optional)
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

func readPolicyAt(root int, components []string, optional bool) (*capturedPolicy, string) {
	parent, err := unix.Dup(root)
	if err != nil {
		return nil, "internal"
	}
	for _, component := range components[:len(components)-1] {
		next, err := unix.Openat(parent, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			code := classifyAtError(parent, component, err, false)
			_ = unix.Close(parent)
			return nil, code
		}
		unix.Close(parent)
		parent = next
	}
	leaf := components[len(components)-1]
	fd, err := unix.Openat(parent, leaf, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) && optional {
			var st unix.Stat_t
			if e := unix.Fstatat(parent, leaf, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(e, syscall.ENOENT) {
				return &capturedPolicy{absent: true, parent: parent, leaf: leaf, fd: -1}, ""
			} else if e == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
				_ = unix.Close(parent)
				return nil, "policy_symlink"
			}
		}
		code := classifyAtError(parent, leaf, err, true)
		_ = unix.Close(parent)
		return nil, code
	}
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		_ = unix.Close(fd)
		_ = unix.Close(parent)
		return nil, "policy_unreadable"
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		_ = unix.Close(parent)
		return nil, "policy_not_regular"
	}
	if before.Size > MaxSourceBytes {
		_ = unix.Close(fd)
		_ = unix.Close(parent)
		return nil, "policy_too_large"
	}
	if testPolicyReadHook != nil {
		testPolicyReadHook(fd)
	}
	b := make([]byte, 0, before.Size)
	buf := make([]byte, 32<<10)
	for {
		n, e := unix.Read(fd, buf)
		if n > 0 {
			if len(b)+n > MaxSourceBytes {
				_ = unix.Close(fd)
				_ = unix.Close(parent)
				return nil, "policy_too_large"
			}
			b = append(b, buf[:n]...)
		}
		if e == nil && n > 0 {
			continue
		}
		if errors.Is(e, syscall.EINTR) {
			continue
		}
		if n == 0 {
			break
		}
		_ = unix.Close(fd)
		_ = unix.Close(parent)
		return nil, "policy_unreadable"
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		_ = unix.Close(fd)
		_ = unix.Close(parent)
		return nil, "policy_unreadable"
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || int64(len(b)) != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		_ = unix.Close(fd)
		_ = unix.Close(parent)
		return nil, "policy_changed_during_load"
	}
	var named unix.Stat_t
	if err := unix.Fstatat(parent, leaf, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil || named.Mode&unix.S_IFMT != unix.S_IFREG || named.Dev != after.Dev || named.Ino != after.Ino {
		_ = unix.Close(fd)
		_ = unix.Close(parent)
		return nil, "policy_changed_during_load"
	}
	return &capturedPolicy{bytes: b, parent: parent, leaf: leaf, fd: fd, stat: after}, ""
}

func classifyOpenError(err error, leaf bool) string {
	if errors.Is(err, syscall.ELOOP) {
		return "policy_symlink"
	}
	if errors.Is(err, syscall.ENOENT) {
		return "policy_missing"
	}
	if errors.Is(err, syscall.ENOTDIR) {
		return "policy_not_regular"
	}
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return "policy_unreadable"
	}
	_ = leaf
	return "policy_unreadable"
}

func classifyAtError(parent int, name string, err error, leaf bool) string {
	var st unix.Stat_t
	if e := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); e == nil {
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return "policy_symlink"
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG && leaf {
			return "policy_not_regular"
		}
	}
	return classifyOpenError(err, leaf)
}
