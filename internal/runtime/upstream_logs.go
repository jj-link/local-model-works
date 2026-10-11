package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/errdefs"
)

// Each retained file and owned engine ID has its own checkpoint. Their output
// may interleave differently after reconnect without changing resume semantics.
func (r *upstreamRuntime) CaptureLogs(ctx context.Context, id string, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	r.mu.Lock()
	rec := r.record(id)
	if rec == nil {
		r.mu.Unlock()
		if strings.HasPrefix(id, "~lmw-upstream-") {
			return errdefs.NotFound(fmt.Errorf("upstream.installation_missing: %s", id))
		}
		return r.Runtime.CaptureLogs(ctx, id, cursor, emit)
	}
	if rememberLogSources(rec) {
		if err := r.save(rec); err != nil {
			r.mu.Unlock()
			return err
		}
	}
	id = rec.ID
	files := []*captureFile{
		{path: filepath.Join(r.root, id, "stdout.log"), stream: "stdout"},
		{path: filepath.Join(r.root, id, "stderr.log"), stream: "stderr"},
	}
	if rec.Spec.Upstream.LogFile != "" {
		files = append(files, &captureFile{path: filepath.Join(rec.Workspace, "repository", rec.Spec.Upstream.SourcePath, rec.Spec.Upstream.LogFile), stream: "stdout"})
	}
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		for _, file := range files {
			file.close()
		}
	}()
	type result struct {
		id  string
		err error
	}
	finished := make(chan result)
	type attachment struct {
		stop   context.CancelFunc
		follow bool
	}
	attached := make(map[string]attachment)
	ended := make(map[string]bool)
	sources := make(map[string]bool) // immutable ID -> follow (false retires it)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, file := range files {
			if err := file.drain(ctx, r.root, cursor, emit); err != nil {
				return err
			}
		}
		r.mu.Lock()
		rec = r.records[id]
		clear(sources)
		terminal := rec == nil
		if rec != nil {
			info, err := r.inspect(ctx, rec)
			if err != nil {
				r.mu.Unlock()
				return err
			}
			terminal = (info.State == "exited" || info.State == "dead" || rec.Removed || rec.Stopped || rec.Superseded) && !r.jobAlive(rec)
			if rememberLogSources(rec) {
				if err := r.save(rec); err != nil {
					r.mu.Unlock()
					return err
				}
			}
			for observed, name := range rec.LogSources {
				owner := r.currentOwner(name, observed)
				if owner != nil && owner != rec {
					continue // Another installation owns this actual ID now.
				}
				current := !rec.Superseded && !rec.Removed && rec.Observed[name] == observed
				if current && owner != rec {
					continue // Ambiguous current ownership is not authority.
				}
				sources[observed] = current
			}
		}
		r.mu.Unlock()
		for observed, running := range attached {
			follow, retained := sources[observed]
			if !retained || running.follow != follow {
				running.stop()
			}
		}
		for observed, follow := range sources {
			if attached[observed].stop != nil {
				continue
			}
			if ended[observed] {
				if !follow {
					continue
				}
				info, err := r.Runtime.Inspect(ctx, observed)
				if errdefs.IsNotFound(err) || (err == nil && info.State != "running" && info.State != "restarting") {
					continue
				}
				if err != nil {
					return err
				}
			}
			child, stop := context.WithCancel(ctx)
			attached[observed] = attachment{stop: stop, follow: follow}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer stop()
				var err error
				if follow {
					err = r.Runtime.CaptureLogs(child, observed, cursor, emit)
				} else {
					err = r.Runtime.CaptureLogSnapshot(child, observed, cursor, emit)
				}
				select {
				case finished <- result{id: observed, err: err}:
				case <-ctx.Done():
				}
			}()
		}
		if terminal && len(attached) == 0 {
			// State may become terminal before the durable supervisor's
			// background group exits. Only now is a final file drain safe.
			for _, file := range files {
				if err := file.drain(ctx, r.root, cursor, emit); err != nil {
					return err
				}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-finished:
			delete(attached, result.id)
			// A cancelled follow is not a drained retired source. Its bounded
			// snapshot starts only after this reader has fully returned.
			ended[result.id] = result.err == nil || errdefs.IsNotFound(result.err)
			if result.err != nil && !errors.Is(result.err, context.Canceled) && !errdefs.IsNotFound(result.err) {
				return result.err
			}
		case <-ticker.C:
		}
	}
}

type captureFile struct {
	path   string
	stream string
	file   *os.File
	key    string
	buf    []byte
}

func (f *captureFile) close() {
	if f.file != nil {
		_ = f.file.Close()
		f.file = nil
	}
}

func (f *captureFile) drain(ctx context.Context, root string, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	if f.key == "" {
		f.key = "file:" + f.path
	}
	key := f.key
	// Retain the descriptor between polls: drain a renamed inode before moving
	// to its replacement, rather than discarding its unconsumed tail.
	if f.file != nil {
		if err := f.read(ctx, key, cursor, emit); err != nil {
			return err
		}
	}
	relative, err := filepath.Rel(root, f.path)
	if err != nil {
		return err
	}
	resolved, err := upstreamPath(root, relative, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	next, err := os.Open(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, err := next.Stat()
	if err != nil {
		_ = next.Close()
		return err
	}
	if f.file != nil {
		previous, err := f.file.Stat()
		if err == nil && os.SameFile(previous, stat) {
			_ = next.Close()
			return nil
		}
	}
	f.close()
	f.file = next
	return f.read(ctx, key, cursor, emit)
}

func (f *captureFile) read(ctx context.Context, key string, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	stat, err := f.file.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("logs.source_not_regular: %s", f.path)
	}
	position := cursor(key)
	previous := position
	identity := logFileIdentity(stat)
	if position.FileID != identity || stat.Size() < position.Offset {
		position = LogCursor{FileID: identity}
	}
	if position.Offset > 0 {
		anchor, err := fileLogAnchor(f.file, position.Offset)
		if err != nil {
			return err
		}
		if anchor != position.Anchor {
			// copytruncate can regrow past the old offset between polls.
			position = LogCursor{FileID: identity}
		}
	}
	if stat.Size() == position.Offset {
		if position != previous {
			return emit(LogRecord{Source: key, Stream: f.stream, Cursor: position})
		}
		return nil
	}
	if f.buf == nil {
		f.buf = make([]byte, 256*1024)
	}
	buf := f.buf
	// Drain this size snapshot, not an indefinitely growing file, so a noisy
	// supervisor cannot starve the other retained source files.
	for position.Offset < stat.Size() && ctx.Err() == nil {
		n, anchor, err := readLogFileChunk(f.file, position, buf[:min(int64(len(buf)), stat.Size()-position.Offset)])
		if errors.Is(err, errLogSourceChanged) {
			// Retry this source on the next pass, before consuming any of the
			// replacement generation at an offset belonging to the old one.
			return nil
		}
		if n > 0 {
			position.Offset += int64(n)
			position.Anchor = anchor
			if err := emit(LogRecord{Source: key, Stream: f.stream, Data: buf[:n], Cursor: position}); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) || n == 0 {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func fileLogAnchor(file *os.File, offset int64) (string, error) {
	length := min(offset, 256)
	var buf [256]byte
	if _, err := file.ReadAt(buf[:length], offset-length); err != nil {
		return "", err
	}
	hash := sha256.Sum256(buf[:length])
	return hex.EncodeToString(hash[:]), nil
}

var errLogSourceChanged = errors.New("logs.source_changed")

// Anchor only captured bytes. A second read after the payload can observe a
// truncate/regrow generation that the payload never contained.
func readLogFileChunk(file io.ReaderAt, position LogCursor, data []byte) (int, string, error) {
	var tail [256]byte
	length := int(min(position.Offset, int64(len(tail))))
	if length > 0 {
		if _, err := file.ReadAt(tail[:length], position.Offset-int64(length)); err != nil {
			if errors.Is(err, io.EOF) {
				return 0, "", errLogSourceChanged
			}
			return 0, "", err
		}
		hash := sha256.Sum256(tail[:length])
		if hex.EncodeToString(hash[:]) != position.Anchor {
			return 0, "", errLogSourceChanged
		}
	}
	n, err := file.ReadAt(data, position.Offset)
	if n == 0 {
		return n, "", err
	}
	if length > 0 {
		// Also reject a generation switch between validating the preceding
		// cursor and reading the next payload. Do not certify the new file's
		// suffix at an offset inherited from the old file's prefix.
		var check [256]byte
		if _, checkErr := file.ReadAt(check[:length], position.Offset-int64(length)); checkErr != nil {
			if errors.Is(checkErr, io.EOF) {
				return 0, "", errLogSourceChanged
			}
			return 0, "", checkErr
		}
		if !bytes.Equal(check[:length], tail[:length]) {
			return 0, "", errLogSourceChanged
		}
	}
	if n >= len(tail) {
		copy(tail[:], data[n-len(tail):n])
		length = len(tail)
	} else {
		keep := min(length, len(tail)-n)
		copy(tail[:keep], tail[length-keep:length])
		copy(tail[keep:], data[:n])
		length = keep + n
	}
	hash := sha256.Sum256(tail[:length])
	return n, hex.EncodeToString(hash[:]), err
}
