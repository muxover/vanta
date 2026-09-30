//go:build !linux

package sys

import (
	"errors"
	"net/netip"
	"syscall"
)

var errLinuxOnly = errors.New("only supported on Linux")

func EnableNonlocalBind() error { return errLinuxOnly }

func Tune() []error { return []error{errLinuxOnly} }

type Routes struct{}

func NewRoutes() *Routes { return &Routes{} }

func (r *Routes) Sync(prefixes []netip.Prefix) error {
	if len(prefixes) == 0 {
		return nil
	}
	return errLinuxOnly
}

func (r *Routes) Close() error { return nil }

func DialControl(_, _ string, _ syscall.RawConn) error { return nil }

func ListenControl(_, _ string, _ syscall.RawConn) error { return nil }
