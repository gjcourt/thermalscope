//go:build !linux

package nvmehealth

import (
	"fmt"
	"runtime"
)

// readSMARTLog is unsupported off Linux: the NVMe admin passthrough ioctl is a
// Linux interface. The collector only runs on Linux nodes; this stub exists so
// the package builds and unit tests (which inject a fake reader) run on other
// platforms.
func readSMARTLog(string) ([]byte, error) {
	return nil, fmt.Errorf("nvme SMART log unsupported on %s", runtime.GOOS)
}
