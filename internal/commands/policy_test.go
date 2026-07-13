package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/urfave/cli/v3"
)

func policyTestDeps(load func(string, string, string) (*policy.Snapshot, []policy.Diagnostic)) policyCommandDeps {
	return policyCommandDeps{
		root: func() (string, error) { return "/canonical/project", nil },
		rootID: func(root string) (string, error) {
			if root != "/canonical/project" {
				return "", errors.New("unexpected root")
			}
			return "opaque-root-id", nil
		},
		load: load, evaluate: policy.Evaluate,
	}
}

func commandSnapshot() *policy.Snapshot {
	return &policy.Snapshot{SchemaVersion: 1, ID: strings.Repeat("a", 64), Rules: []policy.Rule{{
		ID: "project.block", Kind: policy.ProcessExec, CommandExact: "secret command",
		Reason: "secret policy reason", Effect: policy.Deny,
		Source: policy.SourceDescriptor{Layer: policy.LayerProject, Path: ".shipmates/policy.yaml", Present: true},
	}}}
}

func TestPolicyCommandParsing(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"policy", "validate"}, "usage: shipmates policy validate"},
		{[]string{"policy", "validate", "backend", "extra"}, "usage: shipmates policy validate"},
		{[]string{"policy", "explain", "backend"}, "usage: shipmates policy explain"},
		{[]string{"policy", "explain", "backend", "extra", "--command-exact", "x"}, "usage: shipmates policy explain"},
	} {
		cmd := &cli.Command{Name: "shipmates", Commands: []*cli.Command{Policy()}}
		err := cmd.Run(context.Background(), append([]string{"shipmates"}, tc.args...))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("args %q: error %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestPolicyExplainFlagIsIsolatedFromSiblingCommands(t *testing.T) {
	called := false
	cmd := &cli.Command{Name: "shipmates", Commands: []*cli.Command{
		Policy(),
		{Name: "server", Commands: []*cli.Command{{Name: "serve", Action: func(context.Context, *cli.Command) error { called = true; return nil }}}},
	}}
	if err := cmd.Run(context.Background(), []string{"shipmates", "server", "serve"}); err != nil {
		t.Fatalf("unrelated command required policy flag: %v", err)
	}
	if !called {
		t.Fatal("sibling command did not run")
	}
}

func TestPolicyValidationOutputContract(t *testing.T) {
	var out bytes.Buffer
	err := runPolicyValidation(&out, "backend", "", false, policyTestDeps(func(root, persona, rootID string) (*policy.Snapshot, []policy.Diagnostic) {
		if persona != "backend" || rootID != "opaque-root-id" {
			t.Fatalf("unexpected load identity: %q %q", persona, rootID)
		}
		return commandSnapshot(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if json.Unmarshal(out.Bytes(), &got) != nil || got["valid"] != true || got["policy_snapshot_id"] != strings.Repeat("a", 64) || len(got) != 3 {
		t.Fatalf("validation output: %s", out.Bytes())
	}
}

func TestPolicyExplainIsBoundedAndDoesNotEchoPolicyTextOrCommand(t *testing.T) {
	var out bytes.Buffer
	err := runPolicyValidation(&out, "backend", "secret command", true, policyTestDeps(func(string, string, string) (*policy.Snapshot, []policy.Diagnostic) {
		return commandSnapshot(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, secret := range []string{"secret command", "secret policy reason"} {
		if strings.Contains(text, secret) {
			t.Fatalf("explanation leaked %q: %s", secret, text)
		}
	}
	var got struct {
		PolicyEffect   string `json:"policy_effect"`
		RuntimeOutcome string `json:"runtime_outcome"`
		ReasonCode     string `json:"reason_code"`
		MatchedRules   []struct {
			RuleID string `json:"rule_id"`
		} `json:"matched_rules"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PolicyEffect != "deny" || got.RuntimeOutcome != "refused" || got.ReasonCode != "matched_deny" || len(got.MatchedRules) != 1 || got.MatchedRules[0].RuleID != "project.block" {
		t.Fatalf("explanation output: %s", out.Bytes())
	}
}

func TestPolicyInvalidAndMissingUseOnlySafeDiagnostics(t *testing.T) {
	for _, diagnostic := range []policy.Diagnostic{
		{SchemaVersion: 1, Code: "policy_missing", Layer: policy.LayerPersona, Path: ".shipmates/policies/backend.yaml", Message: "required policy source is absent"},
		{SchemaVersion: 1, Code: "policy_invalid_yaml", Layer: policy.LayerProject, Path: ".shipmates/policy.yaml", Line: 2, Column: 3, Message: "policy document is invalid YAML"},
	} {
		var out bytes.Buffer
		err := runPolicyValidation(&out, "backend", "", false, policyTestDeps(func(string, string, string) (*policy.Snapshot, []policy.Diagnostic) {
			return nil, []policy.Diagnostic{diagnostic}
		}))
		if err == nil || err.Error() != "policy validation failed" {
			t.Fatalf("error = %v", err)
		}
		if strings.Contains(out.String(), "/canonical/project") || strings.Contains(out.String(), "command_exact") {
			t.Fatalf("unsafe diagnostic: %s", out.Bytes())
		}
		var got policyDiagnosticOutput
		if json.Unmarshal(out.Bytes(), &got) != nil || got.Valid || len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != diagnostic.Code {
			t.Fatalf("diagnostic output: %s", out.Bytes())
		}
	}
}
