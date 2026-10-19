package netwatch

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// WaitMountChange returns nil once this mount namespace's table changes after
// the call began, e.g. when the root mount service attaches the NAS share. The
// kernel flags /proc/self/mountinfo with POLLPRI; nothing is mounted or read
// beneath any mount point. Cancellation wakes the poll through eventfd.
func WaitMountChange(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.Open("/proc/self/mountinfo", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return err
	}
	defer unix.Close(wake)
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(cancelDone)
		var one [8]byte
		binary.NativeEndian.PutUint64(one[:], 1)
		for {
			_, err := unix.Write(wake, one[:])
			if !errors.Is(err, unix.EINTR) {
				return
			}
		}
	})
	defer func() {
		if !stop() {
			<-cancelDone
		}
	}()
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLPRI}, {Fd: int32(wake), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(poll, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if poll[0].Revents&(unix.POLLPRI|unix.POLLERR) != 0 {
			return nil
		}
		return fmt.Errorf("unexpected mount table readiness")
	}
}
