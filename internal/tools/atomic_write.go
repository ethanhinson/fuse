package tools

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxSymlinkHops matches the kernel's usual limit.
const maxSymlinkHops = 40

// writeFileAtomic replaces path's content so that a crash or signal at any moment
// leaves either the old file or the new one, never a truncated mix: it writes a temp
// file beside the target, fsyncs it, and renames it into place.
//
// It keeps the guarantees os.WriteFile gave that a plain rename would lose:
//
//   - A symlink is followed, so the link survives and its target is what changes.
//     That includes a dangling link, which writing through creates the target of.
//   - An existing file keeps its permission bits. A new one is 0o644 under the umask.
//   - A read-only file stays refused. Rename needs permission on the directory, not
//     the file, so without the check below it would succeed.
//   - Something that is not a regular file (a device, a FIFO, a directory) is opened
//     and written directly. Renaming over /dev/null would replace the device, and
//     there is no prior content to protect.
//
// Owner, hard links and extended attributes do not survive: the replacement is a
// new file.
func writeFileAtomic(path string, data []byte) error {
	target, err := resolveWriteTarget(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return os.WriteFile(target, data, 0o644)
	case err == nil:
		probe, perr := os.OpenFile(target, os.O_WRONLY, 0)
		if perr != nil {
			return perr
		}
		probe.Close()
	case !os.IsNotExist(err):
		return err
	}

	prefix := parentPrefix(target)
	tmp, err := createTemp(prefix)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	// Only an existing file has bits to restore. For a new one this would override
	// the umask that createTemp's open just applied.
	if info != nil {
		if err := tmp.Chmod(info.Mode().Perm()); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	committed = true
	// Makes the rename itself durable. Some filesystems refuse fsync on a directory,
	// and the file is already in place by then, so failure here is not an error.
	if d, err := os.Open(prefix + "."); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// createTemp makes the temp file with 0o644 rather than os.CreateTemp's 0o600,
// because open(2) is the one way to get the process's umask applied without the
// process-wide, racy umask(2). The name does not embed the target's, which may
// already be as long as a name can be.
func createTemp(prefix string) (*os.File, error) {
	for tries := 0; ; tries++ {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		name := prefix + ".fuse-tmp-" + hex.EncodeToString(b)
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) && tries < 10 {
			continue
		}
		return f, err
	}
}

// parentPrefix returns path up to and including its last separator. It stands in
// for filepath.Dir and Join here because they Clean, which resolves "dir/.."
// lexically, and that is wrong when dir is a symlink.
func parentPrefix(path string) string {
	return path[:strings.LastIndexByte(path, filepath.Separator)+1]
}

// resolveWriteTarget follows path while it is a symlink and returns the file a write
// through it would reach, which need not exist. filepath.EvalSymlinks will not do:
// it fails on a dangling link.
func resolveWriteTarget(path string) (string, error) {
	for hop := 0; hop < maxSymlinkHops; hop++ {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		link, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(link) {
			path = link
		} else {
			path = parentPrefix(path) + link
		}
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}
