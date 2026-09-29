// Package command runs programs without a shell. Arguments are always a
// list, never text a shell would split.
package command

import (
	"bufio"
	"context"
	"os/exec"
	"time"
)

const timeout = 2 * time.Minute

// Command runs a program and returns its standard output.
type Command func(ctx context.Context, name string, args ...string) ([]byte, error)

// Stream runs a program until ctx ends or the program exits, and calls line
// for each line of its standard output.
type Stream func(ctx context.Context, line func(string), name string, args ...string) error

// Exec runs programs with the given environment and stops them after 2
// minutes.
func Exec(env []string) Command {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		command := exec.CommandContext(ctx, name, args...)
		command.Env = env
		return command.Output()
	}
}

// ExecStream runs long-lived programs with the given environment.
func ExecStream(env []string) Stream {
	return func(ctx context.Context, line func(string), name string, args ...string) error {
		command := exec.CommandContext(ctx, name, args...)
		command.Env = env
		out, err := command.StdoutPipe()
		if err != nil {
			return err
		}
		if err := command.Start(); err != nil {
			return err
		}
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			line(scanner.Text())
		}
		return command.Wait()
	}
}
