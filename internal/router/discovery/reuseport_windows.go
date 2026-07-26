//go:build windows

package discovery

import (
	"strconv"
	"syscall"
)

// reusePortControl (Windows): SO_REUSEADDR ayni portu paylasmaya izin verir.
func reusePortControl(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return serr
}

func itoa(n int) string { return strconv.Itoa(n) }
