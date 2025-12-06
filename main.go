package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/Noooste/azuretls-client"
)

var session *azuretls.Session

func formatURL(u *url.URL) string {
	u.Path = strings.TrimLeft(u.Path, "/")
	u.Scheme = "https"
	return u.String()
}

func copyHeaders(h http.Header) map[string][]string {
	headers := make(map[string][]string, len(h))

	for k, value := range h {
		if k != "user-agent" {
			headers[k] = value
		}
	}

	return headers
}

func newSession() *azuretls.Session {
	s := azuretls.NewSession()

	s.UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
	s.EnableLog()
	// s.EnableDump()

	return s
}

func proxyHandler(w http.ResponseWriter, req *http.Request) {
	if session == nil {
		session = newSession()
	}

	resp, err := session.Do(&azuretls.Request{
		Method:     http.MethodGet,
		Url:        formatURL(req.URL),
		Header:     copyHeaders(req.Header),
		IgnoreBody: true,
	})
	if err != nil {
		fmt.Println(err)
		w.WriteHeader(502)
		io.WriteString(w, "Proxy Error")
		return
	}

	defer resp.RawBody.Close()

	for k, v := range resp.Header {
		if k == "Content-Length" {
			continue
		}
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.RawBody)
	if err != nil {
		fmt.Println("Write error: ", err)
	}
}

func main() {
	defer session.Close()

	http.HandleFunc("/", proxyHandler)
	fmt.Println("Serving at localhost:7000")
	log.Fatal(http.ListenAndServe("localhost:7000", nil))
}
