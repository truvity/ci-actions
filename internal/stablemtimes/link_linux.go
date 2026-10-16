//go:build linux

package stablemtimes

import (
	"errors"
	"syscall"
	"time"
	"unsafe"
)

// setLinkTime sets the access and modification time of the symlink itself
// (utimensat with AT_SYMLINK_NOFOLLOW).
func setLinkTime(path string, t time.Time) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	ts := [2]syscall.Timespec{syscall.NsecToTimespec(t.UnixNano()), syscall.NsecToTimespec(t.UnixNano())}
	const atSymlinkNoFollow = 0x100
	dirfd := -100 // AT_FDCWD
	_, _, e := syscall.Syscall6(syscall.SYS_UTIMENSAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&ts[0])), atSymlinkNoFollow, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func isUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EPERM)
}
