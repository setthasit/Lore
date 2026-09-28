//go:build !windows

package plugexec

import (
	"os"
	"syscall"
)

func interrupt(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
