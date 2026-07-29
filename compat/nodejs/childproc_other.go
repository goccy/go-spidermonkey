//go:build !unix

package nodejs

import "os/exec"

// Process groups are a Unix concept; elsewhere a child is killed on its own.
func isolateProcessGroup(cmd *exec.Cmd) {}
func killProcessGroup(pid int)          {}
