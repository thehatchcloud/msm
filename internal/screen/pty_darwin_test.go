package screen

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY returns a pseudo-terminal pair for the attach test, without
// cgo or a third-party pty package: grantpt/unlockpt/ptsname are the
// TIOCPTYGRANT/TIOCPTYUNLK/TIOCPTYGNAME ioctls on XNU.
func openPTY() (primary, replica *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	primary = os.NewFile(uintptr(fd), "/dev/ptmx")
	fail := func(err error) (*os.File, *os.File, error) {
		primary.Close()
		return nil, nil, err
	}
	for _, req := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := unix.IoctlSetInt(fd, req, 0); err != nil {
			return fail(err)
		}
	}
	var name [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		return fail(errno)
	}
	path := string(name[:bytes.IndexByte(name[:], 0)])
	replica, err = os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	return primary, replica, nil
}
