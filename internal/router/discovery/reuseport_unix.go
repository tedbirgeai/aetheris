//go:build !windows

package discovery

import (
	"strconv"
	"syscall"
)

// reusePortControl, sokete SO_REUSEADDR (ve mumkunse SO_REUSEPORT) uygular;
// boylece ayni host'ta birden cok dugum ayni broadcast portunu paylasabilir.
func reusePortControl(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
			serr = e
			return
		}
		// SO_REUSEPORT (Linux=15, BSD/darwin=0x200) best-effort; hata onemli degil.
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1)
	})
	if err != nil {
		return err
	}
	return serr
}

func itoa(n int) string { return strconv.Itoa(n) }
