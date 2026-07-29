//go:build unix

package nodejs

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts a spawned child in its own process group so that
// closing the runtime can kill the child AND anything it spawned. Without it a
// grandchild — `sh -c` starting something that outlives the shell — keeps the
// parent's stdout pipe open, and a runner that waits on that pipe never
// returns even though every test has finished. That is how one leaked
// subprocess wedged a whole suite shard.
func isolateProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup kills the child and its descendants. A negative pid names
// the group.
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
