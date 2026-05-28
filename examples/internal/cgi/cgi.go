// Package cgi provides shared CGI/1.1 helpers for php-wasm wazero examples.
package cgi

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// WriteCGIResponse parses a raw php-cgi output (headers + blank line + body)
// and writes it to the HTTP response.
func WriteCGIResponse(w http.ResponseWriter, response, requestID string) {
	headerEnd := strings.Index(response, "\r\n\r\n")
	bodyOffset := 4
	if headerEnd < 0 {
		headerEnd = strings.Index(response, "\n\n")
		bodyOffset = 2
	}

	if headerEnd < 0 {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, response)
		return
	}

	headerSection := response[:headerEnd]
	body := response[headerEnd+bodyOffset:]

	statusCode := http.StatusOK
	for _, line := range strings.Split(headerSection, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])

		switch strings.ToLower(name) {
		case "status":
			if code, err := strconv.Atoi(strings.SplitN(value, " ", 2)[0]); err == nil {
				statusCode = code
			}
		case "location":
			w.Header().Set("Location", value)
		case "set-cookie":
			w.Header().Add("Set-Cookie", value)
		case "content-type":
			w.Header().Set("Content-Type", value)
		default:
			w.Header().Set(name, value)
		}
	}

	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(statusCode)
	io.WriteString(w, body)
}

// BlockedByPath returns true when urlPath matches a blocked prefix or name.
func BlockedByPath(urlPath string, blockedPaths []string) bool {
	clean := filepath.Clean(urlPath)
	rel := strings.TrimPrefix(clean, string(os.PathSeparator))
	for _, blocked := range blockedPaths {
		if rel == blocked || strings.HasPrefix(rel, blocked) {
			return true
		}
	}
	return false
}

// HasDotfileSegment returns true when any path segment starts with a dot
// (e.g. .htaccess, .env). The segment "." itself is allowed.
func HasDotfileSegment(urlPath string) bool {
	for _, seg := range strings.Split(filepath.Clean(urlPath), string(os.PathSeparator)) {
		if strings.HasPrefix(seg, ".") && seg != "." {
			return true
		}
	}
	return false
}
