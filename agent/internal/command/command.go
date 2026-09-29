// Package command runs programs without a shell. Arguments are always a
// list, never text a shell would split.
package command

import (
	"bufio"
	"context"
	"errors"
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
func Exec(env []string) Command { return ExecFor(env, timeout) }

// ExecFor runs programs with the given environment and stops them after
// limit. A limit of 0 never stops them.
func ExecFor(env []string, limit time.Duration) Command {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if limit > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, limit)
			defer cancel()
		}
		command := exec.CommandContext(ctx, name, args...)
		command.Env = env
		return command.Output()
	}
}

// ExecStream runs long-lived programs with the given environment. When a
// program fails, the error holds the end of its standard error, as Exec's does.
func ExecStream(env []string) Stream {
	return func(ctx context.Context, line func(string), name string, args ...string) error {
		command := exec.CommandContext(ctx, name, args...)
		command.Env = env
		stderr := &tail{}
		command.Stderr = stderr
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
		err = command.Wait()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			exit.Stderr = stderr.data
		}
		return err
	}
}

// tail keeps the last 4 KiB written to it.
type tail struct{ data []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.data = append(t.data, p...)
	if extra := len(t.data) - 4096; extra > 0 {
		t.data = t.data[extra:]
	}
	return len(p), nil
}
