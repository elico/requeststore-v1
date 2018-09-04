package requeststore

import (
	"net/http"
	"net/url"
	"time"
)

// Request ---
type Request struct {
	RequestTime time.Time
	method      string
	proto       string
	url         url.URL
	header      http.Header
}

// NewRequest ---
func NewRequest(req *http.Request) *Request {
	return &Request{
		method: req.Method,
		url:    *req.URL,
		proto:  req.Proto,
		header: req.Header,
	}
}

func (r *Request) Header() http.Header {
	return r.header
}

/*
func NewRequest(reqUrl url.URL, reqMethod string, reqHeader http.Header, reqProto string) *Request {
	return &Request{
		header: reqHeader,
		method: reqMethod,
		proto:  reqProto,
		url:    reqUrl,
	}
}
*/
