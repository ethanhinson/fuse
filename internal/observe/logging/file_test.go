package logging

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFileSinkReopenSuccessAndFailureKeepsOldSink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fuse.log")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Write([]byte("old\n")); err != nil {
		t.Fatal(err)
	}
	rotated := path + ".1"
	if err = os.Rename(path, rotated); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.Reopen(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "missing", "fuse.log")
	s.SetPath(bad)
	if err = s.Reopen(); err == nil {
		t.Fatal("reopen failure expected")
	}
	if _, err = s.Write([]byte("still-new\n")); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "new\nstill-new\n" {
		t.Fatalf("active=%q", b)
	}
	b, _ = os.ReadFile(rotated)
	if string(b) != "old\n" {
		t.Fatalf("rotated=%q", b)
	}
}

func TestFileSinkConcurrentWriteReopenShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fuse.log")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = s.Write([]byte("{}\n"))
				if j%25 == 0 {
					_ = s.Reopen()
				}
			}
		}()
	}
	wg.Wait()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}
