package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

func New(path string, maxBytes int64, backups int) (*slog.Logger, io.Closer, error) {
	var output io.Writer = os.Stdout
	var closer io.Closer
	if path != "" {
		file, err := newRotatingFile(path, maxBytes, backups)
		if err != nil {
			return nil, nil, err
		}
		output = io.MultiWriter(os.Stdout, file)
		closer = file
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo})), closer, nil
}

type rotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	file     *os.File
	size     int64
}

func newRotatingFile(path string, maxBytes int64, backups int) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	writer := &rotatingFile{path: path, maxBytes: maxBytes, backups: backups}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (r *rotatingFile) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size > 0 && r.size+int64(len(data)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := r.file.Write(data)
	r.size += int64(written)
	return written, err
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

func (r *rotatingFile) rotate() error {
	if err := r.file.Close(); err != nil {
		return err
	}
	r.file = nil
	for index := r.backups - 1; index >= 1; index-- {
		oldPath := r.path + "." + strconv.Itoa(index)
		newPath := r.path + "." + strconv.Itoa(index+1)
		if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
			_ = r.open()
			return err
		}
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		_ = r.open()
		return err
	}
	return r.open()
}

func (r *rotatingFile) open() error {
	file, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("secure log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	r.file = file
	r.size = info.Size()
	return nil
}
