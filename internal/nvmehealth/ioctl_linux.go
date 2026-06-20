//go:build linux

package nvmehealth

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// nvmeAdminGetLogPage is the NVMe admin opcode for "Get Log Page".
const nvmeAdminGetLogPage = 0x02

// smartLogID is the SMART/Health Information log page identifier.
const smartLogID = 0x02

// nvmeIoctlAdminCmd is the _IOWR('N', 0x41, struct nvme_admin_cmd) request
// number; the struct is 72 bytes (see nvmePassthruCmd).
const nvmeIoctlAdminCmd = 0xC0484E41

// nsidAll addresses the whole controller (all namespaces) for the log page.
const nsidAll = 0xFFFFFFFF

// nvmePassthruCmd mirrors Linux's `struct nvme_admin_cmd`
// (uapi/linux/nvme_ioctl.h), 72 bytes. Only the fields we set are used.
type nvmePassthruCmd struct {
	opcode      uint8
	flags       uint8
	rsvd1       uint16
	nsid        uint32
	cdw2        uint32
	cdw3        uint32
	metadata    uint64
	addr        uint64
	metadataLen uint32
	dataLen     uint32
	cdw10       uint32
	cdw11       uint32
	cdw12       uint32
	cdw13       uint32
	cdw14       uint32
	cdw15       uint32
	timeoutMs   uint32
	result      uint32
}

// readSMARTLog issues a Get Log Page admin command for the SMART/Health page on
// the controller character device at devPath and returns the raw log bytes.
func readSMARTLog(devPath string) ([]byte, error) {
	f, err := os.OpenFile(devPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", devPath, err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, smartLogLen)

	// cdw10: bits 0-7 = log page id; bits 16-31 = number of dwords minus one.
	const numDwords = smartLogLen / 4
	cdw10 := uint32(smartLogID) | uint32(numDwords-1)<<16

	cmd := nvmePassthruCmd{
		opcode:  nvmeAdminGetLogPage,
		nsid:    nsidAll,
		addr:    uint64(uintptr(unsafe.Pointer(&buf[0]))),
		dataLen: smartLogLen,
		cdw10:   cdw10,
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(nvmeIoctlAdminCmd),
		uintptr(unsafe.Pointer(&cmd)),
	)
	if errno != 0 {
		return nil, fmt.Errorf("nvme admin ioctl on %s: %w", devPath, errno)
	}
	return buf, nil
}
