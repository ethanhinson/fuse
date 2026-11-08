package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxTaskFileBytes bounds --task-file, so a wrong path (a log, a binary) fails
// clearly instead of becoming a huge prompt.
const maxTaskFileBytes = 1 << 20

// readTaskFile reads a one-shot task from path, or from stdin when path is "-".
// A task on the command line is visible to every process on the machine, and an
// agent that kills processes by matching their command line can match its own
// harness; a file keeps it out of the process list.
func readTaskFile(path string) (string, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxTaskFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxTaskFileBytes {
		return "", fmt.Errorf("%s is larger than %d bytes", path, maxTaskFileBytes)
	}
	task := strings.TrimSpace(string(b))
	if task == "" {
		return "", errors.New("the task is empty")
	}
	return task, nil
}
