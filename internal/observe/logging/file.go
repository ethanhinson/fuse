package logging

import (
	"errors"
	"os"
	"sync"
)

// FileSink supports rename/create/reopen rotation. copytruncate is intentionally
// unsupported because it cannot preserve the atomic whole-record lifecycle.
type FileSink struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	closed bool
}

func OpenFile(path string) (*FileSink, error) {
	f, err := openLogFile(path)
	if err != nil {
		return nil, err
	}
	return &FileSink{path: path, file: f}, nil
}
func openLogFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("logging: sink is not a regular file")
	}
	return f, nil
}
func (s *FileSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, os.ErrClosed
	}
	return s.file.Write(p)
}

// Reopen validates the new descriptor before locking out writes. Once locked,
// all prior writes have drained; the swap and old close occur exactly once.
func (s *FileSink) Reopen() error {
	s.mu.Lock()
	path := s.path
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return os.ErrClosed
	}
	next, err := openLogFile(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = next.Close()
		return os.ErrClosed
	}
	old := s.file
	s.file = next
	return old.Close()
}
func (s *FileSink) SetPath(path string) { s.mu.Lock(); defer s.mu.Unlock(); s.path = path }
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.file.Close()
}
