package tools

import (
	"context"
	"path/filepath"
	"sync"
	"time"
)

// fileInfo tracks what this agent knows about a file on disk.
type fileInfo struct {
	readFully   bool
	readModTime time.Time
	readSize    int64

	writtenModTime time.Time
	writtenSize    int64
}

// FileTracker records per-file read/write state for one agent. A child agent
// gets its own tracker so reads and writes are isolated per agent.
type FileTracker struct {
	mu    sync.Mutex
	files map[string]*fileInfo
}

// NewFileTracker returns an empty tracker.
func NewFileTracker() *FileTracker {
	return &FileTracker{files: map[string]*fileInfo{}}
}

// MarkRead records that path was read. full is true only for an unranged
// read (no start_line/end_line). A partial read (full=false) does NOT
// authorize overwrite but DOES count toward the edit_file staleness check.
func (ft *FileTracker) MarkRead(path string, full bool, modTime time.Time, size int64) {
	if ft == nil {
		return
	}
	resolved, err := resolvePath(path)
	if err != nil {
		return
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	fi, ok := ft.files[resolved]
	if !ok {
		fi = &fileInfo{}
		ft.files[resolved] = fi
	}
	if full {
		fi.readFully = true
	}
	fi.readModTime = modTime
	fi.readSize = size
}

// MarkWritten records that path was written by this agent. Writing a file
// is treated as knowing its content fully, so it also authorizes overwrite.
func (ft *FileTracker) MarkWritten(path string, modTime time.Time, size int64) {
	if ft == nil {
		return
	}
	resolved, err := resolvePath(path)
	if err != nil {
		return
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	fi, ok := ft.files[resolved]
	if !ok {
		fi = &fileInfo{}
		ft.files[resolved] = fi
	}
	fi.readFully = true
	fi.writtenModTime = modTime
	fi.writtenSize = size
	// Also bump the read modtime so the edit staleness check uses the
	// latest known state regardless of whether it came from read or write.
	if modTime.After(fi.readModTime) {
		fi.readModTime = modTime
		fi.readSize = size
	}
}

// AuthorizeOverwrite reports whether this agent may overwrite path. It is
// allowed when the agent has read the file in full or was the last writer.
func (ft *FileTracker) AuthorizeOverwrite(path string) bool {
	if ft == nil {
		return false
	}
	resolved, err := resolvePath(path)
	if err != nil {
		return false
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	fi, ok := ft.files[resolved]
	if !ok {
		return false
	}
	return fi.readFully
}

// HasState reports whether this agent has any recorded state for path (read
// or written). Used by edit_file to decide whether to perform a staleness
// check.
func (ft *FileTracker) HasState(path string) bool {
	if ft == nil {
		return false
	}
	resolved, err := resolvePath(path)
	if err != nil {
		return false
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	_, ok := ft.files[resolved]
	return ok
}

// IsStale reports whether path has changed on disk since this agent last
// read or wrote it, based on modification time and size. Returns false
// when the agent has no state for this file.
func (ft *FileTracker) IsStale(path string, modTime time.Time, size int64) bool {
	if ft == nil {
		return false
	}
	resolved, err := resolvePath(path)
	if err != nil {
		return false
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	fi, ok := ft.files[resolved]
	if !ok {
		return false
	}
	// Use the most recent known state (write may be newer than read).
	knownModTime := fi.readModTime
	knownSize := fi.readSize
	if fi.writtenModTime.After(knownModTime) {
		knownModTime = fi.writtenModTime
		knownSize = fi.writtenSize
	}
	return !modTime.Equal(knownModTime) || size != knownSize
}

// resolvePath returns path's real location, so a relative path, a symlink and its
// target all share tracker state. A file that does not exist yet resolves through
// its nearest existing parent.
func resolvePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest), nil
		}
		if dir == filepath.Dir(dir) {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

// context key for per-agent file tracker.
type fileTrackerKeyType struct{}

var fileTrackerKey fileTrackerKeyType

// WithFileTracker returns ctx carrying ft.
func WithFileTracker(ctx context.Context, ft *FileTracker) context.Context {
	return context.WithValue(ctx, fileTrackerKey, ft)
}

// FileTrackerFrom returns the file tracker ctx carries, or nil.
func FileTrackerFrom(ctx context.Context) *FileTracker {
	ft, _ := ctx.Value(fileTrackerKey).(*FileTracker)
	return ft
}
