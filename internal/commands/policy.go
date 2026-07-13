package commands

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Policy exposes read-only inspection of the strict project-local policy.
// It deliberately has no dependency on sessions, approvals, or either runtime.
func Policy() *cli.Command {
	return &cli.Command{
		Name:  "policy",
		Usage: "validate and inspect the Codex-native project policy",
		Commands: []*cli.Command{
			{
				Name:      "validate",
				Usage:     "validate the complete policy and print its semantic identity",
				ArgsUsage: "<persona>",
				Action: func(ctx context.Context, c *cli.Command) error {
					if c.Args().Len() != 1 {
						return errors.New("usage: shipmates policy validate <persona>")
					}
					return runPolicyValidation(c.Writer, c.Args().First(), "", false, defaultPolicyCommandDeps())
				},
			},
			{
				Name:      "explain",
				Usage:     "explain the bounded policy decision for one exact command",
				ArgsUsage: "<persona>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "command-exact", Usage: "opaque exact command to evaluate"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					// Keep requiredness local to this action. urfave/cli v3 applies a
					// Required subcommand flag while parsing unrelated sibling commands,
					// which must not make live/server operations depend on policy explain.
					if c.Args().Len() != 1 || !c.IsSet("command-exact") {
						return errors.New("usage: shipmates policy explain <persona> --command-exact <command>")
					}
					return runPolicyValidation(c.Writer, c.Args().First(), c.String("command-exact"), true, defaultPolicyCommandDeps())
				},
			},
		},
	}
}

type policyCommandDeps struct {
	root     func() (string, error)
	rootID   func(string) (string, error)
	load     func(string, string, string) (*policy.Snapshot, []policy.Diagnostic)
	evaluate func(*policy.Snapshot, policy.Request) policy.Explanation
}

func defaultPolicyCommandDeps() policyCommandDeps {
	return policyCommandDeps{
		root: func() (string, error) {
			root, err := os.Getwd()
			if err != nil {
				return "", err
			}
			return filepath.EvalSymlinks(root)
		},
		rootID: project.ScopeID, load: policy.Load, evaluate: policy.Evaluate,
	}
}

type policyDiagnosticOutput struct {
	SchemaVersion int          `json:"schema_version"`
	Valid         bool         `json:"valid"`
	Diagnostics   []policyDiag `json:"diagnostics"`
}

type policyDiag struct {
	SchemaVersion int          `json:"schema_version"`
	Code          string       `json:"code"`
	Layer         policy.Layer `json:"layer,omitempty"`
	Path          string       `json:"path,omitempty"`
	Line          int          `json:"line,omitempty"`
	Column        int          `json:"column,omitempty"`
	RuleID        string       `json:"rule_id,omitempty"`
	Message       string       `json:"message"`
}

type policyValidationOutput struct {
	SchemaVersion    int    `json:"schema_version"`
	Valid            bool   `json:"valid"`
	PolicySnapshotID string `json:"policy_snapshot_id"`
}

type policyExplanationOutput struct {
	SchemaVersion    int                 `json:"schema_version"`
	PolicySnapshotID string              `json:"policy_snapshot_id"`
	RequestKind      policy.RequestKind  `json:"request_kind"`
	PolicyEffect     policy.Effect       `json:"policy_effect"`
	RuntimeOutcome   string              `json:"runtime_outcome"`
	ReasonCode       string              `json:"reason_code"`
	MatchedRules     []policyMatchOutput `json:"matched_rules"`
	OmittedMatches   int                 `json:"omitted_matches"`
}

type policyMatchOutput struct {
	Layer  policy.Layer  `json:"layer"`
	Path   string        `json:"path"`
	RuleID string        `json:"rule_id"`
	Effect policy.Effect `json:"effect"`
}

func runPolicyValidation(out io.Writer, persona, command string, explain bool, deps policyCommandDeps) error {
	if err := project.ValidatePersonaName(persona); err != nil {
		return errors.New("invalid persona")
	}
	root, err := deps.root()
	if err != nil {
		return errors.New("cannot resolve project root")
	}
	rootID, err := deps.rootID(root)
	if err != nil {
		return errors.New("cannot identify project root")
	}
	snapshot, diagnostics := deps.load(root, persona, rootID)
	if snapshot == nil || len(diagnostics) != 0 {
		if len(diagnostics) == 0 {
			diagnostics = []policy.Diagnostic{{SchemaVersion: policy.SchemaVersion, Code: "internal", Message: "policy could not be validated"}}
		}
		safe := make([]policyDiag, 0, len(diagnostics))
		for _, d := range diagnostics {
			safe = append(safe, policyDiag{d.SchemaVersion, d.Code, d.Layer, d.Path, d.Line, d.Column, d.RuleID, d.Message})
		}
		if encodeErr := json.NewEncoder(out).Encode(policyDiagnosticOutput{policy.SchemaVersion, false, safe}); encodeErr != nil {
			return encodeErr
		}
		return errors.New("policy validation failed")
	}
	if !explain {
		return json.NewEncoder(out).Encode(policyValidationOutput{policy.SchemaVersion, true, snapshot.ID})
	}
	e := deps.evaluate(snapshot, policy.Request{Kind: policy.ProcessExec, CommandExact: command})
	matches := make([]policyMatchOutput, 0, len(e.MatchedRules))
	for _, match := range e.MatchedRules {
		matches = append(matches, policyMatchOutput{match.Layer, match.Path, match.ID, match.Effect})
	}
	return json.NewEncoder(out).Encode(policyExplanationOutput{
		e.SchemaVersion, e.PolicySnapshotID, e.RequestKind, e.PolicyEffect, "refused",
		e.ReasonCode, matches, e.OmittedMatches,
	})
}
