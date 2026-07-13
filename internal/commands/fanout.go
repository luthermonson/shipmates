package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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
			inventory, err := project.CanonicalPersonaInventory(".")
			if err != nil {
				return err
			}
			byName := make(map[string]*project.InstalledPersona, len(inventory))
			for i := range inventory {
				byName[inventory[i].Name] = &inventory[i]
			}

			results := make([]fanoutResult, len(personas))
			var wg sync.WaitGroup
			for i, persona := range personas {
				wg.Add(1)
				go func(i int, persona string) {
					defer wg.Done()
					installed := byName[persona]
					if installed == nil {
						results[i] = fanoutResult{persona: persona, err: fmt.Errorf("persona %q is not installed", persona)}
						return
					}
					out, err := oneShotDelegateInstalled(ctx, installed, prompt)
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

// oneShotDelegate runs one canonical Codex persona with isolated output.
func oneShotDelegate(ctx context.Context, persona, prompt string) ([]byte, error) {
	installed, err := project.CanonicalPersonaAt(".", persona)
	if err != nil {
		return nil, err
	}
	return oneShotDelegateInstalled(ctx, installed, prompt)
}

func oneShotDelegateInstalled(ctx context.Context, installed *project.InstalledPersona, prompt string) ([]byte, error) {
	cfg, err := project.ResolvePersonaConfig(installed.Name)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = dispatchCodexInstalled(ctx, installed, prompt, false, cfg, &buf, &buf)
	return buf.Bytes(), err
}
