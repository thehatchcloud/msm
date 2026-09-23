package screen

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openPTY returns a pseudo-terminal pair for the attach test, without
// cgo or a third-party pty package.
func openPTY() (primary, replica *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	primary = os.NewFile(uintptr(fd), "/dev/ptmx")
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		primary.Close()
		return nil, nil, err
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		primary.Close()
		return nil, nil, err
	}
	replica, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		primary.Close()
		return nil, nil, err
	}
	return primary, replica, nil
}
