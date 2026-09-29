package system

import "github.com/iwaneo/hostbeacon/agent/internal/command"

// Command runs a program without a shell and returns its standard output.
type Command = command.Command

// Exec runs programs with the given environment, without a shell, and
// stops them after 2 minutes.
func Exec(env []string) Command { return command.Exec(env) }
