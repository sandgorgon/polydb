package authn

import (
	"errors"
	"net"
	"os/user"
	"strconv"
)

// PeerUser returns the OS user name of the process at the other end of
// a Unix socket (SO_PEERCRED on Linux, LOCAL_PEERCRED on macOS).
func PeerUser(c *net.UnixConn) (string, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return "", err
	}
	var uid int
	var credErr error
	if err := raw.Control(func(fd uintptr) { uid, credErr = peerUID(int(fd)) }); err != nil {
		return "", err
	}
	if credErr != nil {
		return "", credErr
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", errors.New("authn: unknown peer user id " + strconv.Itoa(uid))
	}
	return u.Username, nil
}
