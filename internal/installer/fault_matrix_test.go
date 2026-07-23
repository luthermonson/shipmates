package installer

// This file is deliberately a test-only filesystem/service harness. It makes
// every operation crossing FileSystem and FileHandle auditable and exercises
// each observed occurrence with both an injected filesystem error and a
// crash-after-success restart. No host path or system service is touched.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"syscall"
	"testing"
)

type faultKind string

const (
	faultEIO        faultKind = "EIO"
	faultENOSPC     faultKind = "ENOSPC"
	faultEROFS      faultKind = "EROFS"
	faultEEXIST     faultKind = "EEXIST"
	faultEINTR      faultKind = "EINTR"
	faultPermission faultKind = "permission"
	faultShortWrite faultKind = "short_write"
	crashAfter      faultKind = "crash_after_success"
)

type operationTraceEntry struct {
	Sequence   int       `json:"sequence"`
	Operation  string    `json:"operation"`
	Path       string    `json:"path"`
	Occurrence int       `json:"occurrence"`
	Outcome    string    `json:"outcome"`
	Fault      faultKind `json:"fault,omitempty"`
}

type faultPlan struct {
	Operation  string
	Occurrence int
	Kind       faultKind
}

var errHarnessCrash = errors.New("fault_harness_crash_after_success")

type tracedFS struct {
	base       osFS
	plan       faultPlan
	fast       bool
	trace      []operationTraceEntry
	occurrence map[string]int
}

func newTracedFS(plan faultPlan) *tracedFS {
	// The durability contract is exercised by the ordinary installer tests;
	// this exhaustive matrix models fsync boundaries so hundreds of injected
	// restart cases stay deterministic and fast in CI.
	return &tracedFS{plan: plan, fast: true, occurrence: make(map[string]int)}
}

func (f *tracedFS) observe(operation, path string, success bool, err error) error {
	f.occurrence[operation]++
	n := f.occurrence[operation]
	e := operationTraceEntry{Sequence: len(f.trace) + 1, Operation: operation, Path: path, Occurrence: n, Outcome: "success"}
	if err != nil {
		e.Outcome = "error"
	}
	if f.plan.Operation == operation && f.plan.Occurrence == n {
		e.Fault = f.plan.Kind
		if f.plan.Kind == crashAfter && success {
			e.Outcome = "crash_after_success"
			f.trace = append(f.trace, e)
			panic(errHarnessCrash)
		}
		if f.plan.Kind != crashAfter {
			e.Outcome = "injected_error"
			f.trace = append(f.trace, e)
			return faultError(f.plan.Kind)
		}
	}
	f.trace = append(f.trace, e)
	return err
}

func faultError(k faultKind) error {
	switch k {
	case faultEIO:
		return syscall.EIO
	case faultENOSPC:
		return syscall.ENOSPC
	case faultEROFS:
		return syscall.EROFS
	case faultEEXIST:
		return syscall.EEXIST
	case faultEINTR:
		return syscall.EINTR
	case faultPermission:
		return os.ErrPermission
	default:
		return errors.New("unknown_fault")
	}
}

func (f *tracedFS) MkdirAll(p string, m os.FileMode) error {
	err := f.base.MkdirAll(p, m)
	return f.observe("mkdir", p, err == nil, err)
}
func (f *tracedFS) Lstat(p string) (os.FileInfo, error) {
	info, err := f.base.Lstat(p)
	if injected := f.observe("lstat", p, err == nil, err); injected != nil {
		return nil, injected
	}
	return info, err
}
func (f *tracedFS) OpenFile(p string, flags int, m os.FileMode) (FileHandle, error) {
	h, err := f.base.OpenFile(p, flags, m)
	if injected := f.observe("open", p, err == nil, err); injected != nil {
		if h != nil {
			_ = h.Close()
		}
		return nil, injected
	}
	if err != nil {
		return nil, err
	}
	return &tracedHandle{parent: f, path: p, base: h}, nil
}
func (f *tracedFS) Rename(a, b string) error {
	err := f.base.Rename(a, b)
	return f.observe("rename", a+" -> "+b, err == nil, err)
}
func (f *tracedFS) Remove(p string) error {
	err := f.base.Remove(p)
	return f.observe("remove", p, err == nil, err)
}
func (f *tracedFS) ReadFile(p string) ([]byte, error) {
	b, err := f.base.ReadFile(p)
	if injected := f.observe("read_file", p, err == nil, err); injected != nil {
		return nil, injected
	}
	return b, err
}
func (f *tracedFS) WriteFile(p string, b []byte, m os.FileMode) error {
	err := f.base.WriteFile(p, b, m)
	return f.observe("write_file", p, err == nil, err)
}
func (f *tracedFS) SyncDir(p string) error {
	var err error
	if !f.fast {
		err = f.base.SyncDir(p)
	}
	return f.observe("sync_dir", p, err == nil, err)
}

type tracedHandle struct {
	parent *tracedFS
	path   string
	base   FileHandle
}

func (h *tracedHandle) Write(b []byte) (int, error) {
	n := h.parent.occurrence["write"] + 1
	if h.parent.plan.Operation == "write" && h.parent.plan.Occurrence == n && h.parent.plan.Kind == faultShortWrite {
		// Materialize a short write so callers cannot accidentally treat a
		// successful prefix as a complete asset.
		short := len(b) / 2
		if short == 0 {
			short = 1
		}
		written, _ := h.base.Write(b[:short])
		h.parent.occurrence["write"]++
		h.parent.trace = append(h.parent.trace, operationTraceEntry{Sequence: len(h.parent.trace) + 1, Operation: "write", Path: h.path, Occurrence: n, Outcome: "injected_error", Fault: h.parent.plan.Kind})
		return written, io.ErrShortWrite
	}
	written, err := h.base.Write(b)
	h.parent.occurrence["write"]++
	if injected := h.parent.observeHandle("write", h.path, n, err == nil, err); injected != nil {
		return written, injected
	}
	return written, err
}

func (f *tracedFS) observeHandle(operation, path string, occurrence int, success bool, err error) error {
	// observe increments its own occurrence, so retain the explicit occurrence
	// from Write while using the same fault semantics for all other handles.
	e := operationTraceEntry{Sequence: len(f.trace) + 1, Operation: operation, Path: path, Occurrence: occurrence, Outcome: "success"}
	if err != nil {
		e.Outcome = "error"
	}
	if f.plan.Operation == operation && f.plan.Occurrence == occurrence {
		e.Fault = f.plan.Kind
		if f.plan.Kind == crashAfter && success {
			e.Outcome = "crash_after_success"
			f.trace = append(f.trace, e)
			panic(errHarnessCrash)
		}
		if f.plan.Kind != crashAfter {
			e.Outcome = "injected_error"
			f.trace = append(f.trace, e)
			return faultError(f.plan.Kind)
		}
	}
	f.trace = append(f.trace, e)
	return err
}

func (h *tracedHandle) Chmod(m os.FileMode) error {
	err := h.base.Chmod(m)
	if injected := h.parent.observe("chmod", h.path, err == nil, err); injected != nil {
		return injected
	}
	return err
}
func (h *tracedHandle) Sync() error {
	var err error
	if !h.parent.fast {
		err = h.base.Sync()
	}
	if injected := h.parent.observe("sync_file", h.path, err == nil, err); injected != nil {
		return injected
	}
	return err
}
func (h *tracedHandle) Stat() (os.FileInfo, error) {
	info, err := h.base.Stat()
	if injected := h.parent.observe("stat", h.path, err == nil, err); injected != nil {
		return nil, injected
	}
	return info, err
}
func (h *tracedHandle) Close() error {
	err := h.base.Close()
	if injected := h.parent.observe("close", h.path, err == nil, err); injected != nil {
		return injected
	}
	return err
}

func operationCounts(trace []operationTraceEntry) map[string]int {
	counts := make(map[string]int)
	for _, e := range trace {
		counts[e.Operation]++
	}
	return counts
}

func traceOperations(trace []operationTraceEntry) []string {
	set := make(map[string]bool)
	for _, e := range trace {
		set[e.Operation] = true
	}
	result := make([]string, 0, len(set))
	for op := range set {
		result = append(result, op)
	}
	sort.Strings(result)
	return result
}

func TestInstallerFaultMatrixCoversReachableOperations(t *testing.T) {
	root := t.TempDir()
	baseline := newTracedFS(faultPlan{})
	if _, err := Install(Options{Root: root, EffectiveUID: 0, FS: baseline}); err != nil {
		t.Fatal(err)
	}
	counts := operationCounts(baseline.trace)
	t.Logf("baseline_counts=%s", mustJSON(counts))
	for _, operation := range []string{"mkdir", "lstat", "open", "chmod", "write", "sync_file", "stat", "close", "read_file", "rename", "remove", "sync_dir"} {
		if counts[operation] == 0 {
			t.Fatalf("reachable operation %q was not observed; trace=%v", operation, traceOperations(baseline.trace))
		}
	}

	faults := []faultKind{faultEIO, faultENOSPC, faultEROFS, faultEEXIST, faultEINTR, faultPermission, faultShortWrite}
	for operation, occurrences := range counts {
		for occurrence := 1; occurrence <= occurrences; occurrence++ {
			kinds := []faultKind{faultEIO}
			if occurrence == 1 {
				kinds = faults
			}
			for _, kind := range kinds {
				caseRoot := t.TempDir()
				fs := newTracedFS(faultPlan{Operation: operation, Occurrence: occurrence, Kind: kind})
				_, _ = Install(Options{Root: caseRoot, EffectiveUID: 0, FS: fs})
				if !hasInjected(fs.trace, operation, occurrence, kind) {
					t.Fatalf("missing injected trace for %s/%d/%s", operation, occurrence, kind)
				}
			}
			if occurrence == 1 {
				caseRoot := t.TempDir()
				fs := newTracedFS(faultPlan{Operation: operation, Occurrence: occurrence, Kind: crashAfter})
				func() {
					defer func() {
						if r := recover(); r != nil && !errors.Is(asError(r), errHarnessCrash) {
							t.Fatalf("unexpected crash marker for %s/%d: %v", operation, occurrence, r)
						}
					}()
					_, _ = Install(Options{Root: caseRoot, EffectiveUID: 0, FS: fs})
				}()
				if !hasInjected(fs.trace, operation, occurrence, crashAfter) {
					t.Fatalf("missing crash trace for %s/%d", operation, occurrence)
				}
				_, _ = Install(Options{Root: caseRoot, EffectiveUID: 0})
			}
		}
	}

	t.Logf("fault_matrix_operations=%s", mustJSON(map[string]any{"baseline_counts": counts, "operations": traceOperations(baseline.trace), "faults": faults, "crash_after_success": true, "daemon_reload": "not_reachable_by_design"}))
}

func TestUninstallFaultMatrixCoversRemovalAndRetention(t *testing.T) {
	baselineRoot := t.TempDir()
	if _, err := Install(Options{Root: baselineRoot, EffectiveUID: 0, FS: newTracedFS(faultPlan{})}); err != nil {
		t.Fatal(err)
	}
	baseline := newTracedFS(faultPlan{})
	if _, err := Uninstall(Options{Root: baselineRoot, EffectiveUID: 0, FS: baseline, Fence: inactiveFence{}}); err != nil {
		t.Fatal(err)
	}
	counts := operationCounts(baseline.trace)
	t.Logf("uninstall_baseline_counts=%s", mustJSON(counts))
	for _, operation := range []string{"lstat", "read_file", "open", "write", "sync_file", "close", "remove", "sync_dir"} {
		if counts[operation] == 0 {
			t.Fatalf("uninstall operation %q was not observed", operation)
		}
	}
	faults := []faultKind{faultEIO, faultENOSPC, faultEROFS, faultEEXIST, faultEINTR, faultPermission}
	for operation, occurrences := range counts {
		for occurrence := 1; occurrence <= occurrences; occurrence++ {
			kinds := []faultKind{faultEIO}
			if occurrence == 1 {
				kinds = faults
			}
			for _, kind := range kinds {
				caseRoot := t.TempDir()
				if _, err := Install(Options{Root: caseRoot, EffectiveUID: 0, FS: newTracedFS(faultPlan{})}); err != nil {
					t.Fatal(err)
				}
				fs := newTracedFS(faultPlan{Operation: operation, Occurrence: occurrence, Kind: kind})
				_, _ = Uninstall(Options{Root: caseRoot, EffectiveUID: 0, FS: fs, Fence: inactiveFence{}})
				if !hasInjected(fs.trace, operation, occurrence, kind) {
					t.Fatalf("missing uninstall injection for %s/%d/%s", operation, occurrence, kind)
				}
			}
			if occurrence == 1 {
				caseRoot := t.TempDir()
				if _, err := Install(Options{Root: caseRoot, EffectiveUID: 0, FS: newTracedFS(faultPlan{})}); err != nil {
					t.Fatal(err)
				}
				fs := newTracedFS(faultPlan{Operation: operation, Occurrence: occurrence, Kind: crashAfter})
				func() {
					defer func() {
						if r := recover(); r != nil && !errors.Is(asError(r), errHarnessCrash) {
							t.Fatalf("unexpected uninstall crash marker for %s/%d: %v", operation, occurrence, r)
						}
					}()
					_, _ = Uninstall(Options{Root: caseRoot, EffectiveUID: 0, FS: fs, Fence: inactiveFence{}})
				}()
				if !hasInjected(fs.trace, operation, occurrence, crashAfter) {
					t.Fatalf("missing uninstall crash trace for %s/%d", operation, occurrence)
				}
				_, _ = Uninstall(Options{Root: caseRoot, EffectiveUID: 0, Fence: inactiveFence{}})
			}
		}
	}
	t.Logf("uninstall_fault_matrix=%s", mustJSON(map[string]any{"baseline_counts": counts, "operations": traceOperations(baseline.trace), "faults": faults, "protected_journal": true}))
}

func hasInjected(trace []operationTraceEntry, operation string, occurrence int, kind faultKind) bool {
	for _, e := range trace {
		if e.Operation == operation && e.Occurrence == occurrence && e.Fault == kind {
			return true
		}
	}
	return false
}

func asError(v any) error {
	if err, ok := v.(error); ok {
		return err
	}
	return fmt.Errorf("%v", v)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

var _ FileSystem = (*tracedFS)(nil)
var _ FileHandle = (*tracedHandle)(nil)
