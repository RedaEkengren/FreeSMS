package server

import (
	"bytes"
	"sync"
)

// Rendering into a buffer before touching the ResponseWriter means a template
// error turns into a clean 500 rather than half a page under a 200. The pool
// keeps that from costing an allocation on every request.
var buffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func newBuffer() *bytes.Buffer {
	b := buffers.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func releaseBuffer(b *bytes.Buffer) {
	// A page that grew unusually large is not worth keeping around; letting it
	// go stops one big render from holding the memory for the process's life.
	if b.Cap() > 1<<20 {
		return
	}
	buffers.Put(b)
}
