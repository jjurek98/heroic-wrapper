package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// validateSudo never borrows the TUI's terminal. Only the validation process
// receives the password, over a private stdin pipe; the game still uses sudo -n
// with /dev/null on stdin. Do not capture authentication output in game logs.
// The caller transfers ownership of password, which is cleared on return.
func validateSudo(parent context.Context, sudo string, password []byte, probe bool) error {
	defer clear(password)
	limit := 2 * time.Minute
	args := []string{"-S", "-p", "", "-v"}
	if probe {
		limit = 10 * time.Second
		args = []string{"-n", "-v"}
	}
	ctx, cancel := context.WithTimeout(parent, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, sudo, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	if !probe {
		// One attempt per submission. EOF prevents sudo from waiting for an
		// invisible retry prompt. No secret is put in argv, env or a file.
		input := append(password, '\n')
		defer clear(input)
		cmd.Stdin = bytes.NewReader(input)
	}
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("sudo authentication timed out; retry or cancel")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return errors.New("sudo did not authorize the launch; check your password and sudo permissions")
		}
		return fmt.Errorf("could not run sudo: %w", err)
	}
	return nil
}
