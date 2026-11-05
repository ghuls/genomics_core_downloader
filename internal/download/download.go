// Package download fetches large files from signed URLs with resume support
// and MD5 verification.
package download

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Phase int

const (
	PhaseHashingPartial Phase = iota // re-hashing an existing .part file before resuming
	PhaseDownloading
)

type Progress struct {
	Phase Phase
	Done  int64
	Total int64 // -1 if unknown
}

type Result struct {
	Path     string
	Size     int64
	MD5      string
	MD5Match bool // only meaningful when an expected MD5 was given
	Skipped  bool // destination already existed
	Resumed  bool
}

var client = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	},
}

const maxRetries = 8

// File downloads url to dest. Data is written to dest+".part" and renamed when
// complete, so an interrupted download is resumed on the next call.
func File(ctx context.Context, url, dest, expectedMD5 string, progress func(Progress)) (Result, error) {
	res := Result{Path: dest}
	if st, err := os.Stat(dest); err == nil {
		res.Skipped, res.Size = true, st.Size()
		return res, nil
	}
	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return res, err
	}
	defer f.Close()

	h := md5.New()
	offset, err := hashExisting(ctx, f, h, progress)
	if err != nil {
		return res, err
	}
	res.Resumed = offset > 0

	total := int64(-1)
	for attempt := 0; ; attempt++ {
		var done bool
		offset, total, done, err = fetch(ctx, url, f, h, offset, progress)
		if done {
			break
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var perm permanentError
		if errors.As(err, &perm) || attempt >= maxRetries {
			return res, err
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(time.Duration(1<<min(attempt, 5)) * time.Second):
		}
	}
	if total >= 0 && offset != total {
		return res, fmt.Errorf("size mismatch: got %d bytes, expected %d", offset, total)
	}
	if err := f.Close(); err != nil {
		return res, err
	}
	if err := os.Rename(part, dest); err != nil {
		return res, err
	}
	res.Size = offset
	res.MD5 = hex.EncodeToString(h.Sum(nil))
	res.MD5Match = strings.EqualFold(res.MD5, strings.TrimSpace(expectedMD5))
	return res, nil
}

type permanentError struct{ error }

func hashExisting(ctx context.Context, f *os.File, h hash.Hash, progress func(Progress)) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	if size == 0 {
		return 0, nil
	}
	pw := &progressWriter{w: h, phase: PhaseHashingPartial, total: size, fn: progress}
	n, err := io.CopyBuffer(pw, &ctxReader{ctx: ctx, r: f}, make([]byte, 4<<20))
	pw.report(true)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// fetch downloads from offset to the end. It returns the new offset, the
// expected total size and whether the file is complete.
func fetch(ctx context.Context, url string, f *os.File, h hash.Hash, offset int64, progress func(Progress)) (int64, int64, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return offset, -1, false, permanentError{err}
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return offset, -1, false, err
	}
	defer resp.Body.Close()

	total := int64(-1)
	switch resp.StatusCode {
	case http.StatusOK:
		if offset > 0 { // server ignored the range: start over
			if err := f.Truncate(0); err != nil {
				return offset, -1, false, permanentError{err}
			}
			offset = 0
			h.Reset()
		}
		total = resp.ContentLength
	case http.StatusPartialContent:
		total = totalFromContentRange(resp.Header.Get("Content-Range"))
	case http.StatusRequestedRangeNotSatisfiable:
		// Already have everything.
		if t := totalFromContentRange(resp.Header.Get("Content-Range")); t == offset {
			return offset, t, true, nil
		}
		return offset, -1, false, permanentError{fmt.Errorf("HTTP %s", resp.Status)}
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return offset, -1, false, permanentError{err}
		}
		return offset, -1, false, err
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, total, false, permanentError{err}
	}
	pw := &progressWriter{w: io.MultiWriter(f, h), phase: PhaseDownloading, done: offset, total: total, fn: progress}
	_, err = io.CopyBuffer(pw, resp.Body, make([]byte, 1<<20))
	pw.report(true)
	offset = pw.done
	if err != nil {
		return offset, total, false, err
	}
	if total >= 0 && offset < total {
		return offset, total, false, io.ErrUnexpectedEOF
	}
	return offset, total, true, nil
}

func totalFromContentRange(cr string) int64 {
	// "bytes 100-199/200" or "bytes */200"
	i := strings.LastIndex(cr, "/")
	if i < 0 {
		return -1
	}
	n, err := strconv.ParseInt(cr[i+1:], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

type progressWriter struct {
	w     io.Writer
	phase Phase
	done  int64
	total int64
	fn    func(Progress)
	last  time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	p.report(false)
	return n, err
}

func (p *progressWriter) report(force bool) {
	if p.fn == nil {
		return
	}
	if now := time.Now(); force || now.Sub(p.last) >= 200*time.Millisecond {
		p.last = now
		p.fn(Progress{Phase: p.phase, Done: p.done, Total: p.total})
	}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}
