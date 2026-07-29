package livesession

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// nonTerminalStates are the State constants a session may move between freely.
// Anything else assigned to Session.state — a terminal constant, or a variable
// that could hold one — is a lifecycle transition and belongs in finish.
var nonTerminalStates = map[string]bool{
	"Starting":     true,
	"Idle":         true,
	"Working":      true,
	"Steering":     true,
	"Interrupting": true,
}

// terminalTransitionOwner is the only function allowed to put a session into a
// terminal state. See Session.finish.
const terminalTransitionOwner = "finish"

// shuttingOwners are the only functions allowed to raise s.shutting. Shutdown
// raises it to claim the teardown; finish lowers it when the teardown lands.
// Anything else setting it is a hand-rolled teardown: it makes monitor's EOF
// path skip finish and makes validateRemoteInterruptLocked refuse new work,
// without ever closing the adapter or releasing the dispatch lock.
var shuttingOwners = map[string]bool{"Shutdown": true, terminalTransitionOwner: true}

// TestTerminalStateOnlyEnteredThroughFinish is the mechanical enforcement of
// the invariant the fleet-interrupt path broke twice: a live session becomes
// Stopped or Failed only inside Session.finish, and only Shutdown/finish touch
// s.shutting.
//
// finish is the single place that closes the backend adapter, releases the
// persona's dispatch lock, publishes session.stopped/session.failed and closes
// s.done — and it is guarded by a one-shot sync.Once. A hand-written
// "s.state, s.shutting = Failed, true" therefore looks like a teardown while
// performing none of it: the backend child keeps executing its turn with its
// approval requests unanswered, monitor silently discards its events, Done()
// waiters block forever, a later Shutdown returns nil without closing the
// adapter, and the persona's dispatch lock is never released so the next
// StartIdle fails Busy forever.
//
// The rule is not a style preference and it is not checkable by reading the
// call sites, so it is checked here instead of trusted to review.
func TestTerminalStateOnlyEnteredThroughFinish(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("parsed no package sources; the invariant is unchecked")
	}

	var violations []string
	stateAssignments := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				name := fn.Name.Name
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					assign, ok := n.(*ast.AssignStmt)
					if !ok {
						return true
					}
					for i, lhs := range assign.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok {
							continue
						}
						// Tuple assignment from a single call has no
						// index-matched RHS to inspect; report it rather than
						// silently skipping a possible violation.
						var rhs ast.Expr
						if len(assign.Rhs) == len(assign.Lhs) {
							rhs = assign.Rhs[i]
						}
						switch sel.Sel.Name {
						case "state":
							stateAssignments++
							if name == terminalTransitionOwner {
								continue
							}
							ident, _ := rhs.(*ast.Ident)
							if ident != nil && nonTerminalStates[ident.Name] {
								continue
							}
							violations = append(violations, position(fset, assign.Pos())+": "+name+" assigns "+render(rhs)+" to .state; terminal transitions belong in Session."+terminalTransitionOwner)
						case "shutting":
							if shuttingOwners[name] {
								continue
							}
							ident, _ := rhs.(*ast.Ident)
							if ident != nil && ident.Name == "false" {
								continue
							}
							violations = append(violations, position(fset, assign.Pos())+": "+name+" sets .shutting = "+render(rhs)+"; only Shutdown may claim a teardown and only Session."+terminalTransitionOwner+" may perform one")
						}
					}
					return true
				})
			}
		}
	}
	// Guard against the check quietly matching nothing (renamed field, moved
	// package, parser filter mistake).
	if stateAssignments == 0 {
		t.Fatal("found no assignments to .state; the invariant check is not looking at the right code")
	}
	if len(violations) != 0 {
		sort.Strings(violations)
		t.Fatalf("terminal-lifecycle transitions outside Session.%s:\n\t%s", terminalTransitionOwner, strings.Join(violations, "\n\t"))
	}
}

func position(fset *token.FileSet, pos token.Pos) string {
	p := fset.Position(pos)
	return p.Filename + ":" + itoa(p.Line)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

func render(e ast.Expr) string {
	switch v := e.(type) {
	case nil:
		return "<tuple-assigned value>"
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return render(v.X) + "." + v.Sel.Name
	default:
		return "<non-constant expression>"
	}
}
