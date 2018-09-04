package requeststore

import (
	"net/http"
)

func CopyHeaders(src, dst http.Header) {
	for k, _ := range dst {
		dst.Del(k)
	}

	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}

	if dst.Get("Content-Type") == "" {
		dst.Add("Content-Type", "    ")
	}
}
