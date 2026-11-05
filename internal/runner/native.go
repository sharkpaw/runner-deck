package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sharkpaw/runner-deck/internal/runnerproc"
)

// LogFileName holds a process runner's output in its install directory, one timestamped line at a
// time, in the format of `docker logs --timestamps`, so both modes share one parser.
const LogFileName = "runner.log"

const logRotateBytes = 5 << 20

// ErrWorkBusy means a job is running, so the work directory cannot be emptied.
var ErrWorkBusy = errors.New("a job is running")

// openLog appends to the runner's log, keeping one older file so it cannot grow without limit.
func openLog(installDir string) (*os.File, error) {
	path := filepath.Join(installDir, LogFileName)
	if info, err := os.Stat(path); err == nil && info.Size() > logRotateBytes {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// stampWriter prefixes each complete line with the time it arrived.
type stampWriter struct {
	mu   sync.Mutex
	out  io.Writer
	now  func() time.Time
	rest []byte
}

func (w *stampWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rest = append(w.rest, p...)
	for {
		i := bytes.IndexByte(w.rest, '\n')
		if i < 0 {
			return len(p), nil
		}
		if _, err := fmt.Fprintf(w.out, "%s %s\n", w.now().UTC().Format(time.RFC3339Nano), w.rest[:i]); err != nil {
			return len(p), err
		}
		w.rest = w.rest[i+1:]
	}
}

// ReadLog returns the last maxBytes of a process runner's log, starting at a whole line.
func ReadLog(installDir string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(filepath.Join(installDir, LogFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := info.Size() - maxBytes
	if offset < 0 {
		offset = 0
	}
	data := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if offset > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return data, nil
}

// CleanWork empties a process runner's work directory except the tool cache, which only saves
// downloads. It refuses with ErrWorkBusy while a job runs.
func CleanWork(installDir string) error {
	if runnerproc.WorkerRunning(installDir) {
		return ErrWorkBusy
	}
	work := filepath.Join(installDir, "_work")
	entries, err := os.ReadDir(work)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "_tool" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(work, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
