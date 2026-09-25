package tasks

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

var ErrLedgerLocked = errors.New("task ledger is already open in another manager or process")

func acquireLedgerLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open task ledger lock: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("task ledger lock must be a regular file")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLedgerLocked
		}
		return nil, fmt.Errorf("lock task ledger: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	// Keep the inode at this path after Close; unlinking a locked file permits
	// another owner to create and lock a different inode while this one is live.
	return file, nil
}
