package requeststore

import (
	"bytes"
	//"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	lastModDivisor = 10
	viaPseudonym   = "ms-store"
)

var Clock = func() time.Time {
	return time.Now().UTC()
}

type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

type ByteReadSeekCloser struct {
	*bytes.Reader
}

type IoReaderToReadSeekCloser struct {
	io.ReadCloser
}

func (brsc *ByteReadSeekCloser) Close() error {
	return nil
}

func (iosc *IoReaderToReadSeekCloser) Seek(offset int64, whence int) (int64, error) {
	return offset, nil
}

func (iosc *IoReaderToReadSeekCloser) Close() error {
	return iosc.Close()
}

type Response struct {
	ReadSeekCloser
	RequestTime, ResponseTime time.Time
	header                    http.Header
	statusCode                int
	stale                     bool
}

func NewResponse(statusCode int, body ReadSeekCloser, hdrs http.Header) *Response {
	return &Response{
		ReadSeekCloser: body,
		header:         hdrs,
		statusCode:     statusCode,
	}
}

func NewResponseBytes(statusCode int, b []byte, hdrs http.Header) *Response {
	return &Response{
		ReadSeekCloser: &ByteReadSeekCloser{bytes.NewReader(b)},
		header:         hdrs,
		statusCode:     statusCode,
	}
}

func NewResponseFromHttp(resp *http.Response , body io.ReadCloser) *Response {
	return &Response{
		ReadSeekCloser: &IoReaderToReadSeekCloser{(body)},
		header:         resp.Header,
		statusCode:     resp.StatusCode,
	}
}

func (r *Response) IsNonErrorStatus() bool {
	return r.statusCode >= 200 && r.statusCode < 400
}

func (r *Response) Status() int {
	return r.statusCode
}

func (r *Response) Header() http.Header {
	return r.header
}

func (r *Response) IsStale() bool {
	return r.stale
}

func (r *Response) MarkStale() {
	r.stale = true
}

func (r *Response) LastModified() time.Time {
	var modTime time.Time

	if lastModHeader := r.header.Get("Last-Modified"); lastModHeader != "" {
		if t, err := http.ParseTime(lastModHeader); err == nil {
			modTime = t
		}
	}

	return modTime
}

func (r *Response) Expires() (time.Time, error) {
	if expires := r.header.Get("Expires"); expires != "" {
		return http.ParseTime(expires)
	}

	return time.Time{}, nil
}


func (r *Response) DateAfter(d time.Time) bool {
	if dateHeader := r.header.Get("Date"); dateHeader != "" {
		if t, err := http.ParseTime(dateHeader); err != nil {
			return false
		} else {
			return t.After(d)
		}
	}
	return false
}

func (r *Response) HasValidators() bool {
	if r.header.Get("Last-Modified") != "" || r.header.Get("Etag") != "" {
		return true
	}

	return false
}

func (r *Response) Via() string {
	via := []string{}
	via = append(via, fmt.Sprintf("1.1 %s", viaPseudonym))
	return strings.Join(via, ",")
}
