package main

import (
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Noooste/azuretls-client"
)

var session *azuretls.Session

func hash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

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

func apiProxyHandler(w http.ResponseWriter, req *http.Request) {
	if session == nil {
		session = newSession()
	}

	path, err := url.PathUnescape(req.PathValue("url"))
	if err != nil {
		fmt.Println(err)
		w.WriteHeader(400)
		io.WriteString(w, "Invalid URL")
		return
	}

	url := req.URL
	url.Path = path

	resp, err := session.Do(&azuretls.Request{
		Method:     http.MethodGet,
		Url:        formatURL(url),
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

func picProxyHandler(w http.ResponseWriter, req *http.Request) {
	url, err := url.PathUnescape(req.PathValue("url"))
	if err != nil {
		w.WriteHeader(400)
		io.WriteString(w, "Invalid URL")
		return
	}

	etag := strconv.FormatUint(hash(url), 10)
	if req.Header.Get("If-None-Match") == etag {
		fmt.Println("[PIC] ETag matched:", url)
		w.WriteHeader(304)
		return
	}

	if strings.Contains(req.URL.Path, "orig/") {
		url = url + "?name=orig"
	}

	if !strings.Contains(url, "twimg.com") {
		url = "pbs.twimg.com/" + url
	}

	if !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}

	resp, err := http.DefaultClient.Get(url)
	if err != nil {
		fmt.Println("[PIC] Error:", err)
		w.WriteHeader(502)
		io.WriteString(w, "Proxy Error")
		return
	}

	defer resp.Body.Close()

	for k, v := range resp.Header {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	w.Header().Add("ETag", etag)

	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		fmt.Println("[PIC] Write error:", err)
	}
}

func main() {
	defer session.Close()

	http.HandleFunc("/api/{url...}", apiProxyHandler)
	http.HandleFunc("/pic/{url}", picProxyHandler)
	http.HandleFunc("/pic/orig/{url}", picProxyHandler)
	fmt.Println("Serving at localhost:7000")
	log.Fatal(http.ListenAndServe("localhost:7000", nil))
}
