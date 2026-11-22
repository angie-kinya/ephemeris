//go:build linux

package tty

import (
	"errors"
	"os"
	"testing"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func echoOn(t *testing.T, f *os.File) bool {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag&unix.ECHO != 0
}

func TestWithRawRestoresOnPanic(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if !echoOn(t, tty) {
		t.Fatal("fresh pty should echo")
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic did not propagate")
			}
		}()
		_ = WithRaw(tty, func() error {
			if echoOn(t, tty) {
				t.Fatal("not in raw mode inside fn")
			}
			panic("boom")
		})
	}()
	if !echoOn(t, tty) {
		t.Fatal("terminal left in raw mode after panic")
	}

	want := errors.New("plain error")
	if err := WithRaw(tty, func() error { return want }); err != want {
		t.Fatalf("err = %v", err)
	}
	if !echoOn(t, tty) {
		t.Fatal("terminal left in raw mode after error")
	}
}
