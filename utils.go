package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
)

func checkClose(c io.Closer, err *error) {
	cerr := c.Close()
	if *err == nil {
		*err = cerr
	}
}

func isClientDisconnect(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, context.Canceled)
}

func (s *Server) fetchBody(rawURL string) (body string, err error) {
	resp, err := s.httpClient.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer checkClose(resp.Body, &err)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func copyBody(dst io.Writer, src io.Reader) (int64, error) {
	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	return io.CopyBuffer(dst, src, *bufp)
}

func hash(s string) uint64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

func createHMAC(key, data string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(data))
	full := hex.EncodeToString(mac.Sum(nil))
	return strings.ToUpper(full)[:13]
}
