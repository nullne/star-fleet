package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

type ClaudeBackend struct{}

func (c *ClaudeBackend) Run(ctx context.Context, workdir string, prompt string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, "claude",
		"-p", prompt,
		"--dangerously-skip-permissions",
	)
	cmd.Dir = workdir
	var stderr bytes.Buffer
	if output != nil {
		cmd.Stdout = output
		cmd.Stderr = io.MultiWriter(&stderr, output)
	} else {
		cmd.Stderr = &stderr
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude-code: %s: %w", stderr.String(), err)
	}
	return nil
}
