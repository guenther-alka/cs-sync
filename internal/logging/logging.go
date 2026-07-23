// Package logging provides the minimal cs-sync.log writer (KISS: plain
// print statements, no heredoc-style templating -- guideline.info CODE RULES).
package logging

import (
	"fmt"
	"io"
	"os"
	"time"
)

type Logger struct {
	w io.Writer
}

func New(path string) (*Logger, error) {
	if path == "" {
		return &Logger{w: os.Stdout}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &Logger{w: io.MultiWriter(os.Stdout, f)}, nil
}

func (l *Logger) Printf(format string, args ...any) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(l.w, "%s  %s\n", ts, fmt.Sprintf(format, args...))
}
