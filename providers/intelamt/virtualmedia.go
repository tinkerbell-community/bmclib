// SPDX-License-Identifier: Apache-2.0

package intelamt

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// httpReaderAt presents an HTTP resource as an io.ReaderAt using range
// requests, so an image can be streamed to the host over storage redirection
// without downloading it to disk first. A single rolling chunk is cached,
// which suits the largely sequential reads a boot performs.
type httpReaderAt struct {
	url    string
	client *http.Client
	size   int64

	mu         sync.Mutex
	chunkStart int64
	chunk      []byte
	chunkSize  int64
}

// defaultChunkSize is the window fetched per cache miss.
const defaultChunkSize = 8 << 20 // 8 MiB

// newHTTPReaderAt resolves the resource size and verifies range support.
func newHTTPReaderAt(ctx context.Context, url string, client *http.Client) (*httpReaderAt, error) {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("intelamt: HEAD %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("intelamt: HEAD %s: HTTP %d", url, resp.StatusCode)
	}
	if resp.ContentLength <= 0 {
		return nil, fmt.Errorf("intelamt: %s has no content length", url)
	}
	return &httpReaderAt{url: url, client: client, size: resp.ContentLength, chunkSize: defaultChunkSize}, nil
}

// ReadAt implements io.ReaderAt.
func (r *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	end := off + int64(len(p))
	if off < r.chunkStart || end > r.chunkStart+int64(len(r.chunk)) {
		if err := r.fetch(off); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.chunk[off-r.chunkStart:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// fetch loads a chunk starting at off into the cache.
func (r *httpReaderAt) fetch(off int64) error {
	length := r.chunkSize
	if off+length > r.size {
		length = r.size - off
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, r.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+length-1, 10))
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("intelamt: range GET %s: %w", r.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("intelamt: range GET %s: HTTP %d (server must support range requests)", r.url, resp.StatusCode)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		return fmt.Errorf("intelamt: reading range from %s: %w", r.url, err)
	}
	r.chunkStart = off
	r.chunk = buf
	return nil
}

// Size is the total resource length.
func (r *httpReaderAt) Size() int64 { return r.size }
