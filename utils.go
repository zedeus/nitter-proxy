package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
)

// checkClose closes c and sets *err if there was no prior error.
// Use with defer: defer checkClose(resp.Body, &err)
func checkClose(c io.Closer, err *error) {
	cerr := c.Close()
	if *err == nil {
		*err = cerr
	}
}

func isClientDisconnect(err error) bool {
	// Common cases when client aborts a streaming response.
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return true
	}

	// Unwrap net/http / os errors (net.OpError -> os.SyscallError -> syscall.Errno)
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Sometimes the inner error carries EPIPE/ECONNRESET.
		if errors.Is(opErr.Err, syscall.EPIPE) || errors.Is(opErr.Err, syscall.ECONNRESET) {
			return true
		}
	}

	return false
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

// copyBody streams src to dst using a pooled buffer.
func copyBody(dst io.Writer, src io.Reader) (int64, error) {
	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	return io.CopyBuffer(dst, src, *bufp)
}

func hash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

func createHMAC(key, data string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(data))
	full := hex.EncodeToString(mac.Sum(nil))
	return strings.ToUpper(full)[:13]
}
