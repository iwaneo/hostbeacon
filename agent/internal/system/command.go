package system

import (
	"context"
	"os/exec"
	"time"
)

const commandTimeout = 2 * time.Minute

// Exec runs programs with the given environment, without a shell, and
// stops them after 2 minutes.
func Exec(env []string) Command {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		command := exec.CommandContext(ctx, name, args...)
		command.Env = env
		return command.Output()
	}
}
