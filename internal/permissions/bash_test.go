package permissions

import (
	"reflect"
	"testing"
)

func TestSplitCompound(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"git status", []string{"git status"}},
		{"git status && npm test", []string{"git status", "npm test"}},
		{"a && b || c", []string{"a", "b", "c"}},
		{"a; b; c", []string{"a", "b", "c"}},
		{"a | b", []string{"a", "b"}},
		{"a |& b", []string{"a", "b"}},
		{"a & b", []string{"a", "b"}},
		// Quotes protect operators.
		{`echo "a && b"`, []string{`echo "a && b"`}},
		{`echo 'a; b'`, []string{`echo 'a; b'`}},
		// Subshell $() protects operators.
		{`echo $(a && b)`, []string{`echo $(a && b)`}},
	}
	for _, tc := range cases {
		got := SplitCompound(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SplitCompound(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestStripWrappers(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"timeout 30 npm test", "npm test"},
		{"time make build", "make build"},
		{"nice -n 10 pytest", "pytest"},
		{"nohup ./run.sh", "./run.sh"},
		{"stdbuf -oL -eL cargo test", "cargo test"},
		{"xargs echo hello", "echo hello"},
		// Stacked wrappers.
		{"time timeout 30 make test", "make test"},
		// Not a wrapper — no change.
		{"npm test", "npm test"},
		{"git status", "git status"},
		// NOT stripped: env-runners are not on the wrapper list.
		{"npx foo", "npx foo"},
		{"direnv exec . make", "direnv exec . make"},
	}
	for _, tc := range cases {
		got := stripWrappers(tc.in)
		if got != tc.want {
			t.Errorf("stripWrappers(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsReadOnlyBuiltin(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"ls -la src/", true},
		{"cat README.md", true},
		{"pwd", true},
		{"grep -r foo .", true},
		{"git status", true},
		{"git log --oneline -20", true},
		{"git diff HEAD~", true},
		{"git checkout main", false}, // checkout is not read-only
		{"git commit -m x", false},
		{"rm foo", false},
		{"npm install", false},
		{"", false},
	}
	for _, tc := range cases {
		got, _ := isReadOnlyBuiltin(tc.cmd)
		if got != tc.want {
			t.Errorf("isReadOnlyBuiltin(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestShellSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"git status", []string{"git", "status"}},
		{`echo "hello world"`, []string{"echo", "hello world"}},
		{`echo 'a b c'`, []string{"echo", "a b c"}},
		{"  git   status  ", []string{"git", "status"}},
	}
	for _, tc := range cases {
		got := shellSplit(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("shellSplit(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
