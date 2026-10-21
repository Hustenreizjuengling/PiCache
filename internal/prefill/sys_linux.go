package prefill

import (
	"os"
	"strings"
	"syscall"
)

const oNoFollow = syscall.O_NOFOLLOW

// binaryReplaced reports whether the running executable was replaced on
// disk (an update renamed a new binary over it): /proc/self/exe then names
// the unlinked file with " (deleted)".
func binaryReplaced() bool {
	target, err := os.Readlink("/proc/self/exe")
	return err == nil && strings.HasSuffix(target, " (deleted)")
}
