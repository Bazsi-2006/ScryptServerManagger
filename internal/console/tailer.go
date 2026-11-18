package console

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"
)

// Tailer follows a growing file by polling and feeds new bytes through a
// Sanitizer. It copes with truncation and rotation (rename + recreate) and
// can optionally keep application-owned capture files bounded.
//
// Truncation is recognised by the file becoming shorter than the read
// position. Like tail(1), it cannot notice a file that is truncated and then
// rewritten beyond the old position within a single poll interval; rotation
// by rename is detected reliably through the inode.
type Tailer struct {
	Path string
	// MaxBytes > 0 lets the tailer truncate the file once it has been fully
	// read and has grown past this size. Only set this for files the
	// application owns and whose writers use O_APPEND.
	MaxBytes int64
	// Interval between polls (default 250ms).
	Interval time.Duration
	// IdleFlush emits a pending partial line after this much silence
	// (default 1.5s; negative disables).
	IdleFlush time.Duration

	san      *Sanitizer
	offset   int64
	inode    uint64
	haveFile bool
	lastData time.Time
}

func NewTailer(path string, san *Sanitizer) *Tailer {
	return &Tailer{Path: path, san: san}
}

func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// SkipToEnd positions the tailer at the current end of the file, returning up
// to maxBytes of trailing content (starting at a line boundary) so callers
// can preload recent history.
func (t *Tailer) SkipToEnd(maxBytes int64) []byte {
	f, err := os.Open(t.Path)
	if err != nil {
		t.haveFile = false
		t.offset = 0
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	size := fi.Size()
	t.offset, t.inode, t.haveFile = size, inodeOf(fi), true
	if maxBytes <= 0 || size == 0 {
		return nil
	}
	start := size - maxBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	n, _ := f.ReadAt(buf, start)
	buf = buf[:n]
	if start > 0 {
		// Drop the first, probably partial, line.
		for i, c := range buf {
			if c == '\n' {
				return buf[i+1:]
			}
		}
		return nil
	}
	return buf
}

// Offset returns the current read position.
func (t *Tailer) Offset() int64 { return t.offset }

// Poll reads whatever has been appended since the last call.
func (t *Tailer) Poll() {
	fi, err := os.Stat(t.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.haveFile = false
			t.offset = 0
		}
		t.idle()
		return
	}
	ino := inodeOf(fi)
	if !t.haveFile || ino != t.inode {
		// New or rotated file: start from the beginning.
		t.haveFile, t.inode, t.offset = true, ino, 0
	}
	size := fi.Size()
	if size < t.offset {
		t.offset = 0 // truncated
	}
	if size == t.offset {
		t.idle()
		return
	}
	f, err := os.Open(t.Path)
	if err != nil {
		return
	}
	defer f.Close()
	// If we are hopelessly behind, skip ahead instead of replaying megabytes.
	const maxBacklog = 2 << 20
	if size-t.offset > maxBacklog {
		t.offset = size - maxBacklog/4
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			t.offset += int64(n)
			_, _ = t.san.Write(buf[:n])
			t.lastData = time.Now()
		}
		if err != nil || n == 0 {
			break
		}
	}
	if t.MaxBytes > 0 && t.offset > t.MaxBytes {
		// Everything up to offset has been consumed; start the file over.
		// Writers hold O_APPEND descriptors, so they continue at the new end.
		if st, err := f.Stat(); err == nil && st.Size() == t.offset {
			if os.Truncate(t.Path, 0) == nil {
				t.offset = 0
			}
		}
	}
}

func (t *Tailer) idle() {
	flush := t.IdleFlush
	if flush == 0 {
		flush = 1500 * time.Millisecond
	}
	if flush > 0 && !t.lastData.IsZero() && time.Since(t.lastData) >= flush {
		t.san.Flush()
		t.lastData = time.Time{}
	}
}

// Run polls until ctx is cancelled, then performs a final poll and flush.
func (t *Tailer) Run(ctx context.Context) {
	interval := t.Interval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Poll()
			t.san.Flush()
			return
		case <-tick.C:
			t.Poll()
		}
	}
}
