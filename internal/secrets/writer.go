package secrets

import "io"

// Each Write is scrubbed on its own, so a value split across two calls reaches w intact.
func (s *Sink) Writer(w io.Writer) io.Writer {
	if s == nil {
		return w
	}
	return &scrubWriter{sink: s, w: w}
}

type scrubWriter struct {
	sink *Sink
	w    io.Writer
}

func (sw *scrubWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(sw.w, sw.sink.Scrub(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
