package sandbox

import "bytes"

// maxOutput caps the bytes captured from stdout and stderr, independently.
const maxOutput = 100_000

// cappedBuffer accumulates up to cap bytes, then drops the rest and records a
// truncation note appended once at the tail.
type cappedBuffer struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := w.cap - w.buf.Len(); remaining > 0 {
		if len(p) <= remaining {
			return w.buf.Write(p)
		}
		w.buf.Write(p[:remaining])
		w.truncated = true
	} else {
		w.truncated = true
	}
	return len(p), nil
}

func (w *cappedBuffer) Bytes() []byte {
	if w.truncated {
		return append(w.buf.Bytes(), []byte("\n[output truncated]")...)
	}
	return w.buf.Bytes()
}
