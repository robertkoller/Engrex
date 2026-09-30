package cache

import "net/http"

// flushWriter pushes each write through to the client instead of letting it sit in a
// buffer.
//
// Without this a streamed miss would arrive in whatever chunks net/http decided to flush,
// which for a slow local model means the caller waits and then gets the answer in bursts.
// The point of streaming is that the first tokens show up immediately.
type flushWriter struct {
	writer  http.ResponseWriter
	flusher http.Flusher
	written bool
}

func newFlushWriter(writer http.ResponseWriter) *flushWriter {
	flusher, _ := writer.(http.Flusher)
	return &flushWriter{writer: writer, flusher: flusher}
}

func (stream *flushWriter) Write(data []byte) (int, error) {
	stream.written = true
	written, err := stream.writer.Write(data)
	if stream.flusher != nil {
		stream.flusher.Flush()
	}
	return written, err
}
