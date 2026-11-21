package dockerapi

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// MaxJSONLine bounds one JSON line read from Docker (pull progress, events).
const MaxJSONLine = 64 << 10

// jsonLines reads Docker's newline-delimited JSON streams ("\n" or "\r\n"),
// one line at a time, never holding more than MaxJSONLine bytes of a line.
type jsonLines struct {
	br *bufio.Reader
}

func newJSONLines(r io.Reader) *jsonLines {
	return &jsonLines{br: bufio.NewReaderSize(r, 4<<10)}
}

// next returns the next non-empty line without its terminator. io.EOF means the
// stream ended cleanly at a line boundary; a line over MaxJSONLine is
// ErrLineTooLong and a stream that ends inside a line is io.ErrUnexpectedEOF.
func (j *jsonLines) next() ([]byte, error) {
	for {
		var line []byte
		for {
			chunk, err := j.br.ReadSlice('\n')
			if len(line)+len(chunk) > MaxJSONLine+2 { // +2: "\r\n"
				return nil, ErrLineTooLong
			}
			line = append(line, chunk...)
			if err == nil {
				break
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) {
				if len(line) == 0 {
					return nil, io.EOF
				}
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > MaxJSONLine {
			return nil, ErrLineTooLong
		}
		if len(line) > 0 {
			return line, nil
		}
	}
}
