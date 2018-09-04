package requeststore

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	pathutil "path"
	"strconv"
	"strings"
	"time"

	"github.com/rainycape/vfs"
	"golang.org/x/crypto/md4"
)

const (
	requestPrefix      = "request/"
	headerPrefix       = "header/"
	bodyPrefix         = "body/"
	bodyMetalinkPrefix = "metalink/"
	journalPrefix      = "journal/"
	formatPrefix       = "v1/"
	dummyURL           = "http://dummy"
)

// Returned when a resource doesn't exist
var ErrNotFoundInStore = errors.New("Not found in store")
var ErrFoundWithZeroInStore = errors.New("Found 0 size file in store")
var ErrFoundInStore = errors.New("Found in store")
var ErrFoundInStorePrivate = errors.New("Found in store private")

// Store ---
type Store interface {
	StoreRequest(h http.Request, key string, override bool) error
	StoreResponse(res *Response, key string, override bool) error
	StoreResponseHeader(res *Response, key string, override bool) error
	// StoreBody(h http.Request, key string, override bool) error
	RetrieveRequest(key string) (*http.Request, error)
	RetrieveRequestByHash(hash string) (*http.Request, error)
	RetrieveRequestByFileName(filename string) (*http.Request, error)
	RetrieveResponse(key string) (*Response, error)
	RetrieveResponseHeader(key string) (Header, error)
	// RetrieveResponseMetalink(key string, stored bool) (Metalink, error)
	DelRequest(key string) error
	DelResponse(key string) error
	DelResponseHeader(key string) error
	DelResponseBody(key string) error
	DelByFilename(filename string) error
	WalkRequests() ([]os.FileInfo, []string)
	RetrieveAllRequests() []http.Request
	FetchAndStoreResponse(h http.Request, key string, override bool) error
	HashKey(key string) string
}

type Header struct {
	http.Header
	StatusCode int
}

// store provides a storage mechanism for stored Resources
type store struct {
	fs     vfs.VFS
	stale  map[string]time.Time
	hash   string
	client http.Client
}

var _ Store = (*store)(nil)

// NewVFSStore returns a store backend off the provided VFS
func NewVFSStore(fs vfs.VFS) Store {
	return &store{fs: fs, stale: map[string]time.Time{}}
}

// NewMemoryStore returns an ephemeral store in memory
func NewMemoryStore() Store {
	return NewVFSStore(vfs.Memory())
}

// NewDiskStore returns a disk-backed store
func NewDiskStore(dir string) (Store, error) {
	if err := os.MkdirAll(dir, 0777); err != nil {
		return nil, err
	}
	fs, err := vfs.FS(dir)
	if err != nil {
		return nil, err
	}
	chfs, err := vfs.Chroot("/", fs)
	if err != nil {
		return nil, err
	}
	return NewVFSStore(chfs), nil
}

func (s *store) vfsWrite(path string, r io.Reader) error {
	if err := vfs.MkdirAll(s.fs, pathutil.Dir(path), 0700); err != nil {
		return err
	}

	f, err := s.fs.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return nil
}

func (s *store) vfsCreatePrivateDummy(path string) error {
	if err := vfs.MkdirAll(s.fs, pathutil.Dir(path), 0700); err != nil {
		return err
	}
	f, err := s.fs.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte("dummy")); err != nil {
		return err
	}
	return nil
}

func (s *store) vfsDeletePrivateDummy(path string) error {
	if err := vfs.MkdirAll(s.fs, pathutil.Dir(path), 0700); err != nil {
		return err
	}
	err := s.fs.Remove(path)
	if err != nil {
		if vfs.IsNotExist(err) {
			return ErrNotFoundInStore
		}
		return err
	}
	return nil
}

func (s *store) WalkRequests() ([]os.FileInfo, []string) {
	var walked []os.FileInfo
	var walkedNames []string

	_ = vfs.Walk(s.fs, requestPrefix+formatPrefix, func(fs vfs.VFS, path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		walked = append(walked, info)
		walkedNames = append(walkedNames, path)
		// request, _ := s.RetrieveRequestByFileName(path)
		// fmt.Println(request)
		return nil
	})
	return walked, walkedNames
}

func (s *store) RetrieveAllRequests() []http.Request {
	var requests []http.Request
	// var walkedNames []string

	_ = vfs.Walk(s.fs, requestPrefix+formatPrefix, func(fs vfs.VFS, path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// walked = append(walked, info)
		// walkedNames = append(walkedNames, path)
		request, err := s.RetrieveRequestByFileName(path)
		if err == nil {
			requests = append(requests, *request)
		}
		// fmt.Println(request)
		return nil
	})
	return requests
}

func (s *store) StoreRequest(h http.Request, key string, override bool) error {
	debugf("Got into store.StoreRequest with key => ", key)
	if !override {
		stat, err := s.fs.Stat(requestPrefix + formatPrefix + s.HashKey(key) + ".private")
		if err == nil && stat.Size() > 0 {
			return ErrFoundInStorePrivate
		}
		stat, err = s.fs.Stat(requestPrefix + formatPrefix + s.HashKey(key))
		if err == nil && stat.Size() > 0 {
			return ErrFoundInStore
		}
	}

	hb := &bytes.Buffer{}
	if !h.URL.IsAbs() {
		debugf("Got non Abs url for => ", key, h)
		h.RequestURI = "http://" + h.Host + h.URL.String()
		h.URL, _ = url.Parse(h.RequestURI)
	}
	hb.Write([]byte(fmt.Sprintf("%s %s %s\r\n", h.Method, h.URL.String(), h.Proto)))
	headersToWriter(h.Header, hb)

	s.vfsCreatePrivateDummy(requestPrefix + formatPrefix + s.HashKey(key) + ".private")
	if err := s.vfsWrite(requestPrefix+formatPrefix+s.HashKey(key), bytes.NewReader(hb.Bytes())); err != nil {
		return err
	}
	s.vfsDeletePrivateDummy(requestPrefix + formatPrefix + s.HashKey(key) + ".private")
	return nil
}

// Retrieve returns a stored Resource for the given key
func (s *store) RetrieveRequest(key string) (*http.Request, error) {
	debugf("Got into store.RetrieveRequest with key => ", key)
	return s.RetrieveRequestByHash(s.HashKey(key))
}

// Retrieve returns a stored Resource for the given hash
func (s *store) RetrieveRequestByHash(hash string) (*http.Request, error) {
	debugf("Got into store.RetrieveRequestByHash with hash key => ", hash)
	return s.RetrieveRequestByFileName(requestPrefix + formatPrefix + hash)
}

func (s *store) privateFileExists(file string) error {
	stat, err := s.fs.Stat(file + ".private")
	if err == nil && stat.Size() > 0 {
		return ErrFoundInStorePrivate
	}
	return nil
}

// Retrieve returns a stored Resource for the given a full filename including path in the vfs
func (s *store) RetrieveRequestByFileName(filename string) (*http.Request, error) {
	debugf("Got into store.RetrieveRequestByFileName with filename => ", filename)

	if err := s.privateFileExists(filename); err != nil {
		return nil, err
	}

	f, err := s.fs.Open(filename)
	if err != nil {
		if vfs.IsNotExist(err) {
			return nil, ErrNotFoundInStore
		}
		return nil, err
	}
	defer f.Close()

	fi, err := s.fs.Stat(filename)
	if err != nil {
		return nil, err
	}

	debugf("Request file on disk is for", filename)
	if fi.Size() == 0 {
		return nil, ErrFoundWithZeroInStore
	}

	reqCopy, err := readRequest(bufio.NewReader(f))
	if err != nil {
		if vfs.IsNotExist(err) {
			return nil, ErrNotFoundInStore
		}
		return nil, err
	}

	return reqCopy, nil
}

func (s *store) DelRequest(key string) error {
	debugf("Got into store.DelRequest with key => ", key)

	err := s.DelByFilename(requestPrefix + formatPrefix + s.HashKey(key))
	if err != nil {
		return err
	}
	s.DelByFilename(requestPrefix + formatPrefix + s.HashKey(key) + ".private")
	return nil
}

func (s *store) DelResponse(key string) error {
	debugf("Got into store.DelResponse with key => ", key)

	err := s.DelResponseBody(key)
	if err != nil {
		return err
	}

	err = s.DelResponseHeader(key)
	if err != nil {
		return err
	}

	return nil
}

func (s *store) DelResponseBody(key string) error {
	debugf("Got into store.DelResponse with key => ", key)

	err := s.DelByFilename(bodyPrefix + formatPrefix + s.HashKey(key))
	if err != nil {
		return err
	}
	s.DelByFilename(bodyPrefix + formatPrefix + s.HashKey(key) + ".private")
	return nil
}

func (s *store) DelResponseHeader(key string) error {
	err := s.DelByFilename(headerPrefix + formatPrefix + s.HashKey(key))
	if err != nil {
		return err
	}

	s.DelByFilename(headerPrefix + formatPrefix + s.HashKey(key) + ".private")
	return nil
}

func (s *store) DelByFilename(filename string) error {

	err := s.fs.Remove(filename)
	if err != nil {
		if vfs.IsNotExist(err) {
			return ErrNotFoundInStore
		}
		return err
	}
	return nil
}

func (s *store) StoreResponse(res *Response, key string, overrride bool) error {
	if err := s.storeBody(res, key, overrride); err != nil {
		return err
	}

	if err := s.storeHeader(res.Status(), res.Header(), key, overrride); err != nil {
		return err
	}

	return nil
}

func (s *store) StoreResponseHeader(res *Response, key string, overrride bool) error {
	if err := s.storeHeader(res.Status(), res.Header(), key, overrride); err != nil {
		return err
	}

	return nil
}

func (s *store) RetrieveResponse(key string) (*Response, error) {
	debugf("Got into store.RetrieveResponse with key => ", key)

	if err := s.privateFileExists(bodyPrefix + formatPrefix + s.HashKey(key)); err != nil {
		debugf("Got into store.RetrieveResponse with key => ", key, "privateFileExists")
		return nil, err
	}
	debugf("Got into store.RetrieveResponse with key => ", key, "Step 1")
	//The FD needs to stay open so it would be used later by the retriever function client
	f, err := s.fs.Open(bodyPrefix + formatPrefix + s.HashKey(key))
	if err != nil {
		if vfs.IsNotExist(err) {
			return nil, ErrNotFoundInStore
		}
		debugf("Got into store.RetrieveResponse with key => ", key, "Step 2")
		return nil, err
	}
	debugf("Got into store.RetrieveResponse with key => ", key, "Step 3")
	fi, err := s.fs.Stat(bodyPrefix + formatPrefix + s.HashKey(key))
	if err != nil {
		debugf("Got into store.RetrieveResponse with key => ", key, err)
		return nil, err
	}

	debugf("Response body file on disk is for", key)
	if fi.Size() == 0 {
		return nil, ErrFoundWithZeroInStore
	}

	h, err := s.RetrieveResponseHeader(key)
	if err != nil {
		if vfs.IsNotExist(err) {
			return nil, ErrNotFoundInStore
		}
		return nil, err
	}

	res := NewResponse(h.StatusCode, f, h.Header)

	return res, nil
}

// Retrieve the Status and Headers for a given key path
func (s *store) RetrieveResponseHeader(key string) (Header, error) {
	if err := s.privateFileExists(headerPrefix + formatPrefix + s.HashKey(key)); err != nil {
		return Header{}, err
	}

	path := headerPrefix + formatPrefix + s.HashKey(key)
	f, err := s.fs.Open(path)
	if err != nil {
		if vfs.IsNotExist(err) {
			return Header{}, ErrNotFoundInStore
		}
		return Header{}, err
	}

	return readResponseHeaders(bufio.NewReader(f))
}

func (s *store) storeBody(r io.Reader, key string, override bool) error {
	if !override {
		stat, err := s.fs.Stat(bodyPrefix + formatPrefix + s.HashKey(key) + ".private")
		if err == nil && stat.Size() > 0 {
			return ErrFoundInStorePrivate
		}
		stat, err = s.fs.Stat(bodyPrefix + formatPrefix + s.HashKey(key))
		if err == nil && stat.Size() > 0 {
			return ErrFoundInStore
		}
	}
	debugf("Starting to write BODY", key)
	s.vfsCreatePrivateDummy(bodyPrefix + formatPrefix + s.HashKey(key) + ".private")
	if err := s.vfsWrite(bodyPrefix+formatPrefix+s.HashKey(key), r); err != nil {
		return err
	}
	s.vfsDeletePrivateDummy(bodyPrefix + formatPrefix + s.HashKey(key) + ".private")
	return nil
}

func (s *store) storeHeader(code int, h http.Header, key string, override bool) error {
	if !override {
		stat, err := s.fs.Stat(headerPrefix + formatPrefix + s.HashKey(key) + ".private")
		if err == nil && stat.Size() > 0 {
			return ErrFoundInStorePrivate
		}
		stat, err = s.fs.Stat(headerPrefix + formatPrefix + s.HashKey(key))
		if err == nil && stat.Size() > 0 {
			return ErrFoundInStore
		}
	}

	s.vfsCreatePrivateDummy(headerPrefix + formatPrefix + s.HashKey(key) + ".private")
	hb := &bytes.Buffer{}
	hb.Write([]byte(fmt.Sprintf("HTTP/1.1 %d %s\r\n", code, http.StatusText(code))))
	headersToWriter(h, hb)

	if err := s.vfsWrite(headerPrefix+formatPrefix+s.HashKey(key), bytes.NewReader(hb.Bytes())); err != nil {
		return err
	}
	s.vfsDeletePrivateDummy(headerPrefix + formatPrefix + s.HashKey(key) + ".private")
	return nil
}

func (s *store) HashKey(key string) string {
	debugf("Hasing key \"%s\"", key)
	var h hash.Hash
	switch {
	case strings.ToLower(s.hash) == "sha224":
		h = sha256.New224()
	case strings.ToLower(s.hash) == "sha384":
		h = sha512.New384()
	case strings.ToLower(s.hash) == "sha512":
		h = sha512.New()
	case strings.ToLower(s.hash) == "sha1":
		h = sha1.New()
	case strings.ToLower(s.hash) == "md5":
		h = md5.New()
	case strings.ToLower(s.hash) == "md4":
		h = md4.New()
	case strings.ToLower(s.hash) == "plain":
		return key
	default:
		h = sha256.New()
	}

	io.WriteString(h, key)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (s *store) FetchAndStoreResponse(request http.Request, key string, override bool) error {
	switch {
	case strings.ToLower(key) == "dummy":
		key = request.URL.String()
	default:
	}
	resp, err := s.client.Do(&request)
	if err != nil {
		switch {
		case err != nil && strings.Contains(err.Error(), "REDIRECT!!!"):

		default:
			return err
		}
	}

	err = s.StoreResponse(NewResponseFromHttp(resp, resp.Body), key, override)
	if err != nil && err == ErrFoundInStorePrivate {
		debugf("FetchAndStoreResponse found in cache with override =>", override, ", For key =>", key)
	}
	return err
}

func readRequest(r *bufio.Reader) (*http.Request, error) {
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return dummyReq(), err
	}

	f := strings.SplitN(line, " ", 3)
	if len(f) < 2 {
		return dummyReq(), fmt.Errorf("malformed HTTP request line: %s", line)
	}

	// Here we should parse for url protocol and method
	//f[0] == METHOD
	//f[1] == url
	//f[2] == proto

	if len(f) > 2 {
		debugf(f[0])
		debugf(f[1])
		debugf(f[2])
	} else {
		return dummyReq(), fmt.Errorf("malformed HTTP request line: %s", line)
	}

	mimeHeader, err := tp.ReadMIMEHeader()
	if err != nil {
		return dummyReq(), err
	}

	reqCopy, err := http.NewRequest(f[0], f[1], nil)
	if err != nil {
		return dummyReq(), err
	}
	reqCopy.Header = http.Header(mimeHeader)
	return reqCopy, nil
}

func headersToWriter(h http.Header, w io.Writer) error {
	if err := h.Write(w); err != nil {
		return err
	}
	// ReadMIMEHeader expects a trailing newline
	_, err := w.Write([]byte("\r\n"))
	return err
}

func readResponseHeaders(r *bufio.Reader) (Header, error) {
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return Header{}, err
	}
	if !strings.HasPrefix(line, "HTTP/") {
		// Try to read origial mehtod and url
		// Try to read StoreID

	}
	f := strings.SplitN(line, " ", 3)
	if len(f) < 2 {
		return Header{}, fmt.Errorf("malformed HTTP response: %s", line)
	}
	statusCode, err := strconv.Atoi(f[1])
	if err != nil {
		return Header{}, fmt.Errorf("malformed HTTP status code: %s", f[1])
	}

	mimeHeader, err := tp.ReadMIMEHeader()
	if err != nil {
		return Header{}, err
	}
	return Header{StatusCode: statusCode, Header: http.Header(mimeHeader)}, nil
}

func dummyReq() *http.Request {
	reqCopy, _ := http.NewRequest("GET", dummyURL, nil)
	return reqCopy
}
