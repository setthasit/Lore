//go:build !windows

package plugexec

import (
	"os"
	"syscall"
)

// interrupt asks the plugin to exit. It is the middle step of the escalation:
// stdin EOF is the documented cancel signal, SIGTERM is for a plugin that did
// not take the hint, and SIGKILL follows after the grace.
func interrupt(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
