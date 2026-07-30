package runtime

import (
	"context"
	"errors"
	"fmt"
)

// VerifyCaps checks that a Runtime's declared [Caps] match its behavior, and
// returns one error per discrepancy (nil-length when everything lines up).
//
// It exists because a capability report is the one thing a caller cannot
// verify for itself. Shipmates branches on Caps: it presents a runtime as
// mediating tool calls when Approvals is true, offers steering when Steer is
// true, and reports an environment override as applied when Environment is
// true. A runtime that overstates any of those makes shipmates lie to an
// operator on its behalf. Run this from your runtime's tests:
//
//	if errs := runtime.VerifyCaps(ctx, rt, t.TempDir()); len(errs) > 0 {
//	    for _, err := range errs { t.Error(err) }
//	}
//
// # What it can and cannot prove
//
// It checks the negative direction only: for every capability reported
// false, the corresponding method must return an [*ErrUnsupported], naming
// this runtime, without needing a live session. That is the half that can be
// checked from outside, and it is also the half that matters most — a caller
// that trusts a false report degrades gracefully, while a caller that trusts
// a false-positive report silently loses work.
//
// It cannot check the positive direction. Proving Steer is honored takes a
// live session, a real turn and a model that responds to steering; that
// belongs in the runtime's own integration tests.
//
// scratchDir is a throwaway directory used for the checks that need a project
// root. Nothing should be written to it: every method under test is being
// called for a capability its runtime does not have, and the contract is that
// such a call returns before doing anything.
func VerifyCaps(ctx context.Context, rt Runtime, scratchDir string) []error {
	var errs []error
	name := rt.Name()
	if name == "" {
		errs = append(errs, errors.New("verifycaps: Name() is empty; the runtime must identify itself"))
	}
	caps := rt.Capabilities()

	check := func(feature string, err error) {
		var unsupported *ErrUnsupported
		if !errors.As(err, &unsupported) {
			errs = append(errs, fmt.Errorf(
				"verifycaps: %s reports its capability false but %s returned %v; a false capability must return *ErrUnsupported so callers can degrade",
				name, feature, err))
			return
		}
		if unsupported.Runtime != name {
			errs = append(errs, fmt.Errorf(
				"verifycaps: %s returned ErrUnsupported{Runtime: %q} from %s; it must name the runtime's own Name()",
				name, unsupported.Runtime, feature))
		}
		if unsupported.Feature == "" {
			errs = append(errs, fmt.Errorf(
				"verifycaps: %s returned ErrUnsupported with an empty Feature from %s; an unactionable error is barely better than none",
				name, feature))
		}
	}

	if !caps.Steer {
		check("SteerTurn", rt.SteerTurn(ctx, "no-such-session", "no-such-turn", "steer"))
	}
	if !caps.Interrupt {
		check("InterruptTurn", rt.InterruptTurn(ctx, "no-such-session", "no-such-turn"))
	}
	if !caps.Approvals {
		_, err := rt.ResolveApproval(ctx,
			ApprovalResponse{ID: "no-such-approval"},
			ApprovalDecision{Allow: false, AllowFor: "once"})
		check("ResolveApproval", err)
	}
	if !caps.Environment {
		// The contract for Environment is stricter than "ignore it": a
		// session started under the wrong environment is a session whose
		// output the caller will misread, so the spec must be refused.
		sess, err := rt.StartSession(ctx, SessionSpec{
			Persona:     "verifycaps",
			ProjectDir:  scratchDir,
			Environment: map[string]string{"SHIPMATES_VERIFYCAPS": "1"},
		})
		check("StartSession with SessionSpec.Environment", err)
		if err == nil && sess != nil {
			// It accepted the spec. Do not leave the session running just
			// because the runtime was wrong about itself.
			_ = rt.CloseSession(ctx, sess.ID())
		}
	}
	if !caps.PersonaInstall {
		check("InstallPersona", rt.InstallPersona(ctx, scratchDir, PersonaSpec{Name: "verifycaps"}))
		check("UninstallPersona", rt.UninstallPersona(ctx, scratchDir, "verifycaps"))
	}
	if !caps.MemoryHook {
		check("InstallMemoryHook", rt.InstallMemoryHook(ctx, scratchDir))
	}
	return errs
}
