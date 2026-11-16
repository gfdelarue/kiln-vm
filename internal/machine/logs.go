package machine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

func consoleLogWriter(dataDir, machineID string) io.WriteCloser {
	return &logWriter{logPath: filepath.Join(dataDir, "machines", machineID, "console.log")}
}

type logWriter struct {
	mu      sync.Mutex
	logPath string
	file    *os.File
}

func (lw *logWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	if lw.file == nil {
		if err := os.MkdirAll(filepath.Dir(lw.logPath), 0750); err != nil {
			return 0, fmt.Errorf("create log dir: %w", err)
		}
		f, err := os.OpenFile(lw.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
		if err != nil {
			return 0, fmt.Errorf("open log file: %w", err)
		}
		lw.file = f
	}

	n, err := lw.file.Write(p)
	if err != nil {
		return n, fmt.Errorf("write log: %w", err)
	}
	return n, nil
}

func (lw *logWriter) Close() error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.file != nil {
		err := lw.file.Close()
		lw.file = nil
		if err != nil {
			return fmt.Errorf("close log file: %w", err)
		}
	}
	return nil
}
