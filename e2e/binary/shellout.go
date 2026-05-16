package binary

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CmdResult output of a command, including stdout and stderr
type CmdResult struct {
	Stdout string
	Stderr string
}

func ExecWithContext(ctx context.Context, cmd string, args ...string) (CmdResult, error) {
	return ExecWithContextInDir(ctx, "", cmd, args, nil)
}

func Exec(cmd string, args ...string) (CmdResult, error) {
	return ExecWithContextInDir(context.Background(), "", cmd, args, nil)
}

func ExecWithEnv(env []string, cmd string, args ...string) (CmdResult, error) {
	return ExecWithContextInDir(context.Background(), "", cmd, args, env)
}

func ExecWithContextAndEnv(ctx context.Context, env []string, cmd string, args ...string) (CmdResult, error) {
	return ExecWithContextInDir(ctx, "", cmd, args, env)
}

func ExecWithContextInDir(ctx context.Context, dir, cmd string, args, env []string) (CmdResult, error) {
	command := exec.CommandContext(ctx, cmd, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	res, err := runCmd(command)
	if err != nil {
		return res, fmt.Errorf("executing '%s %s' : %s: %s", cmd, strings.Join(args, " "), err.Error(), res.Stdout+"\n"+res.Stderr)
	}
	return res, nil
}

func runCmd(c *exec.Cmd) (CmdResult, error) {
	if c.Stdout != nil {
		return CmdResult{}, errors.New("exec: Stdout already set")
	}
	if c.Stderr != nil {
		return CmdResult{}, errors.New("exec: Stderr already set")
	}
	var outBuffer bytes.Buffer
	var errBuffer bytes.Buffer
	c.Stdout = &outBuffer
	c.Stderr = &errBuffer
	err := c.Run()
	return CmdResult{outBuffer.String(), errBuffer.String()}, err
}
