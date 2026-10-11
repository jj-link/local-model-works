package runs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"unicode/utf8"
)

// ReadLogTail returns up to 200 recent lines within a 64 KiB window and the
// byte cursor immediately after them. A leading line cut by the window is
// omitted; an unfinished final line is retained for subsequent stream chunks.
func (s *Service) ReadLogTail(runID, deploymentID string, rank int32, stream string) ([]byte, uint64, error) {
	path := s.LogPath(runID, deploymentID, rank, stream)
	if path == "" {
		return nil, 0, errors.New("run log path: unsafe id")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	start := max(int64(0), st.Size()-(64<<10))
	chunk := make([]byte, st.Size()-start)
	n, err := f.ReadAt(chunk, start)
	if err != nil && err != io.EOF {
		return nil, 0, err
	}
	chunk = chunk[:n]
	if complete := completeLogUTF8(chunk); len(complete) < len(chunk) {
		if end, done := s.LogEnded(runID, deploymentID, rank, stream); !done || end > uint64(start)+uint64(n) {
			chunk = complete
			n = len(chunk)
		}
	}
	offset := uint64(start) + uint64(n)
	if start > 0 {
		boundary := bytes.IndexByte(chunk, '\n')
		if boundary < 0 {
			return nil, offset, nil
		}
		chunk = chunk[boundary+1:]
	}
	end := len(chunk)
	if end > 0 && chunk[end-1] == '\n' {
		end--
	}
	lines := 1
	for i := end - 1; i >= 0; i-- {
		if chunk[i] == '\n' {
			if lines == 200 {
				return chunk[i+1:], offset, nil
			}
			lines++
		}
	}
	return chunk, offset, nil
}

// ReadLogText is the live text counterpart of ReadLogTail. Raw byte readers
// retain ReadLog's exact range semantics; SSE readers hold incomplete UTF-8.
func (s *Service) ReadLogText(runID, deploymentID string, rank int32, stream string, offset uint64) (chunk []byte, nextOffset, size uint64, err error) {
	chunk, nextOffset, size, err = s.ReadLog(runID, deploymentID, rank, stream, offset, 0)
	if err != nil {
		return
	}
	if complete := completeLogUTF8(chunk); len(complete) < len(chunk) {
		if end, done := s.LogEnded(runID, deploymentID, rank, stream); !done || end > nextOffset {
			nextOffset -= uint64(len(chunk) - len(complete))
			chunk = complete
		}
	}
	return
}

// JSON log events must not replace a UTF-8 rune split across file reads. Hold
// an incomplete suffix until more bytes arrive (or a confirmed EOF).
func completeLogUTF8(chunk []byte) []byte {
	for i := len(chunk) - 1; i >= max(0, len(chunk)-utf8.UTFMax); i-- {
		if utf8.RuneStart(chunk[i]) {
			if !utf8.FullRune(chunk[i:]) {
				return chunk[:i]
			}
			break
		}
	}
	return chunk
}
