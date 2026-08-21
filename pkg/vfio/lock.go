/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vfio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// lockPrefix deliberately matches the sriov-vfio CNI's lock files: the
	// driver's flocks must hit the same inodes as a CNI binary still
	// installed on the host, so both serialize bridge-VLAN edits during a
	// migration window.
	lockPrefix = "sriov-vfio-cni.lock."

	// lockAcquireTimeout bounds how long a prepare/unprepare waits for the
	// per-bridge lock. flock(2) is released by the kernel when the holder
	// exits, so contention is normally short; a timeout keeps a wedged
	// peer from hanging the kubelet's DRA call forever.
	lockAcquireTimeout = 30 * time.Second
	// lockAcquirePoll is the retry interval while waiting for the lock.
	lockAcquirePoll = 20 * time.Millisecond
)

// ErrLockTimeout is returned when the lock could not be acquired in time.
var ErrLockTimeout = errors.New("timed out waiting for lock")

// Lock represents a held file lock.
type Lock struct {
	file *os.File
}

// AcquireLock obtains an exclusive file lock for the named bridge in dir.
//
// The lock file is intentionally never unlinked. Removing it on release
// would break mutual exclusion: a waiter blocked in flock(2) ends up
// holding a lock on an unlinked inode, while the next process creates a
// fresh inode and locks that one instead, so both would run concurrently.
// The files are zero-length and bounded by the number of bridges on the
// node, so leaving them in place costs nothing.
func AcquireLock(dir, bridge string) (*Lock, error) {
	p := filepath.Join(dir, lockPrefix+bridge)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file %s: %w", p, err)
	}

	deadline := time.Now().Add(lockAcquireTimeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{file: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("failed to acquire lock %s: %w", p, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("failed to acquire lock %s after %s: %w", p, lockAcquireTimeout, ErrLockTimeout)
		}
		time.Sleep(lockAcquirePoll)
	}
}

// Release releases the file lock and closes the file. Closing the
// descriptor already drops the flock; the explicit LOCK_UN keeps the
// intent obvious and releases it even if Close fails.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	if err != nil {
		return fmt.Errorf("failed to close lock file: %w", err)
	}
	return nil
}
