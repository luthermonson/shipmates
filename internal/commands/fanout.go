package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// fanoutResult captures one persona's one-shot delegation outcome. output holds
// the combined stdout+stderr so concurrent runs never interleave on the
// terminal; err is non-nil if the persona was missing or its turn failed.
type fanoutResult struct {
	persona string
	output  []byte
	err     error
}

// Fanout runs the SAME prompt across several personas concurrently — parallel
// one-shot delegations. Each persona's turn mirrors Ask's create-vs-resume
// pattern (see run.go) but captures its output into a private buffer; results
// are printed sequentially once every persona has finished.
func Fanout() *cli.Command {
	return &cli.Command{
		Name:      "fanout",
		Usage:     "run the same prompt across personas in parallel",
		ArgsUsage: "<p1,p2,...> <prompt>",
		Action: func(ctx context.Context, c *cli.Command) error {
			list := c.Args().First()
			prompt := strings.TrimSpace(strings.Join(c.Args().Tail(), " "))

			var personas []string
			for _, p := range strings.Split(list, ",") {
				if p = strings.TrimSpace(p); p != "" {
					personas = append(personas, p)
				}
			}
			if len(personas) == 0 || prompt == "" {
				return errors.New("usage: shipmates fanout <p1,p2,...> <prompt>")
			}

			results := make([]fanoutResult, len(personas))
			var wg sync.WaitGroup
			for i, persona := range personas {
				wg.Add(1)
				go func(i int, persona string) {
					defer wg.Done()
					out, err := oneShotDelegate(ctx, persona, prompt)
					results[i] = fanoutResult{persona: persona, output: out, err: err}
				}(i, persona)
			}
			wg.Wait()

			failures := 0
			for _, r := range results {
				fmt.Printf("==== %s ====\n", r.persona)
				if len(r.output) > 0 {
					_, _ = os.Stdout.Write(r.output)
					if !bytes.HasSuffix(r.output, []byte("\n")) {
						fmt.Println()
					}
				}
				if r.err != nil {
					failures++
					fmt.Printf("error: %v\n", r.err)
				}
				fmt.Println()
			}

			if failures == len(results) {
				return fmt.Errorf("all %d personas failed", len(results))
			}
			return nil
		},
	}
}

// oneShotDelegate runs a single persona's one-shot claude turn, returning its
// combined stdout+stderr. It follows Ask's logic: create the session the first
// time (--session-id/--name), resume it afterward (--resume), and write the
// session marker on a successful create. The persona must be installed.
func oneShotDelegate(ctx context.Context, persona, prompt string) ([]byte, error) {
	if _, err := os.Stat(project.AgentPath(persona)); err != nil {
		return nil, fmt.Errorf("persona %q is not installed", persona)
	}

	name := project.SessionName(persona)
	cfg, _ := project.ResolvePersonaConfig(persona)
	fp := cfg.Fingerprint()

	meta, have := project.ReadSessionMeta(persona)
	fresh := have && meta.ConfigHash != "" && meta.ConfigHash != fp

	var args []string
	if have && !fresh {
		args = []string{"-p", "--resume", name, "--agent", persona}
	} else {
		args = []string{"-p", "--session-id", project.NewUUID(), "--name", name, "--agent", persona}
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	args = append(args, prompt)

	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader("") // immediate EOF — skip claude's ~3s stdin wait
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), err
	}
	if err := project.WriteSessionMeta(persona, name, fp); err != nil {
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}
