package main

import (
	"os"
	"syscall"
	"testing"
)

// TestNativeLinuxLockObservation checks real GNU stat and proc/locks against an
// owned temporary POSIX write lock, including the kernel's device/inode format.
func TestNativeLinuxLockObservation(t *testing.T) {
	m := probeFixture(t, func(path string) func() {
		f, e := os.OpenFile(path, os.O_RDWR, 0600)
		if e != nil {
			t.Fatal(e)
		}
		lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
		if e = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock); e != nil {
			_ = f.Close()
			t.Fatal(e)
		}
		return func() {
			if e := f.Close(); e != nil {
				t.Error(e)
			}
		}
	}, 200, false)
	if got := classify(m, true, true); got != "OBSERVED_HEALTHY_SAMPLE" {
		t.Fatalf("native lock was not observed: %s", got)
	}
}
