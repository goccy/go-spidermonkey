package nodejs_test

import (
	"os"
	"syscall"
)

func osFindProcess() (*os.Process, error) { return os.FindProcess(os.Getpid()) }
func sigUSR1() os.Signal                  { return syscall.SIGUSR1 }
