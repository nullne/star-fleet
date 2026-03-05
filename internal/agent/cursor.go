package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

type CursorBackend struct{}

func (c *CursorBackend) Run(ctx context.Context, workdir string, prompt string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, "cursor",
		"agent",
		"-p", prompt,
		"--trust", "--yolo",
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
		return fmt.Errorf("cursor: %s: %w", stderr.String(), err)
	}
	return nil
}
