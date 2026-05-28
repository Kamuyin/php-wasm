package main

// Minimal FastCGI client for the persistent PHP WASM process.
//
// Wire format: each record is an 8-byte header followed by content + padding.
// We use request ID 1 for every request (single-request-at-a-time via phpMu).

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	fcgiVersion       = 1
	fcgiBeginRequest  = 1
	fcgiAbortRequest  = 2
	fcgiEndRequest    = 3
	fcgiParams        = 4
	fcgiStdin         = 5
	fcgiStdout        = 6
	fcgiStderr        = 7
	fcgiRoleResponder = 1
	fcgiKeepConn      = 1
	fcgiRequestID     = 1
	fcgiMaxContent    = 0xfff8 // max bytes per record content chunk
)

type fcgiHeader struct {
	Version       uint8
	Type          uint8
	RequestIDB1   uint8
	RequestIDB0   uint8
	ContentLenB1  uint8
	ContentLenB0  uint8
	PaddingLength uint8
	Reserved      uint8
}

func writeRecord(w io.Writer, rtype uint8, requestID uint16, data []byte) error {
	contentLen := len(data)
	padding := (8 - (contentLen % 8)) % 8
	hdr := fcgiHeader{
		Version:       fcgiVersion,
		Type:          rtype,
		RequestIDB1:   uint8(requestID >> 8),
		RequestIDB0:   uint8(requestID),
		ContentLenB1:  uint8(contentLen >> 8),
		ContentLenB0:  uint8(contentLen),
		PaddingLength: uint8(padding),
	}
	if err := binary.Write(w, binary.BigEndian, hdr); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if padding > 0 {
		if _, err := w.Write(make([]byte, padding)); err != nil {
			return err
		}
	}
	return nil
}

// encodeNameValue encodes a single CGI name=value pair in FastCGI NV format.
func encodeNameValue(name, value string) []byte {
	nLen, vLen := len(name), len(value)
	var buf bytes.Buffer
	for _, l := range []int{nLen, vLen} {
		if l < 128 {
			buf.WriteByte(byte(l))
		} else {
			b := make([]byte, 4)
			binary.BigEndian.PutUint32(b, uint32(l)|0x80000000)
			buf.Write(b)
		}
	}
	buf.WriteString(name)
	buf.WriteString(value)
	return buf.Bytes()
}

// writeFCGIRequest sends a complete FastCGI request to w (PHP's stdin pipe).
func writeFCGIRequest(w io.Writer, env []string, body []byte) error {
	// BEGIN_REQUEST
	beginBody := make([]byte, 8)
	binary.BigEndian.PutUint16(beginBody[0:2], uint16(fcgiRoleResponder))
	beginBody[2] = fcgiKeepConn
	if err := writeRecord(w, fcgiBeginRequest, fcgiRequestID, beginBody); err != nil {
		return fmt.Errorf("fcgi begin: %w", err)
	}

	// PARAMS — split into chunks if needed
	var paramBuf bytes.Buffer
	for _, kv := range env {
		eqIdx := 0
		for eqIdx < len(kv) && kv[eqIdx] != '=' {
			eqIdx++
		}
		k, v := kv[:eqIdx], ""
		if eqIdx < len(kv) {
			v = kv[eqIdx+1:]
		}
		encoded := encodeNameValue(k, v)
		if paramBuf.Len()+len(encoded) > fcgiMaxContent {
			if err := writeRecord(w, fcgiParams, fcgiRequestID, paramBuf.Bytes()); err != nil {
				return fmt.Errorf("fcgi params: %w", err)
			}
			paramBuf.Reset()
		}
		paramBuf.Write(encoded)
	}
	if err := writeRecord(w, fcgiParams, fcgiRequestID, paramBuf.Bytes()); err != nil {
		return fmt.Errorf("fcgi params flush: %w", err)
	}
	// Empty PARAMS terminates params stream.
	if err := writeRecord(w, fcgiParams, fcgiRequestID, nil); err != nil {
		return fmt.Errorf("fcgi params end: %w", err)
	}

	// STDIN — body in chunks, then empty terminator
	for len(body) > 0 {
		chunk := body
		if len(chunk) > fcgiMaxContent {
			chunk = body[:fcgiMaxContent]
		}
		if err := writeRecord(w, fcgiStdin, fcgiRequestID, chunk); err != nil {
			return fmt.Errorf("fcgi stdin: %w", err)
		}
		body = body[len(chunk):]
	}
	if err := writeRecord(w, fcgiStdin, fcgiRequestID, nil); err != nil {
		return fmt.Errorf("fcgi stdin end: %w", err)
	}

	return nil
}

// fcgiResponse holds the collected STDOUT and STDERR from one FastCGI request.
type fcgiResponse struct {
	stdout []byte
	stderr []byte
}

// readFCGIResponse reads records from r (PHP's stdout pipe) until FCGI_END_REQUEST.
func readFCGIResponse(r io.Reader) (*fcgiResponse, error) {
	var resp fcgiResponse
	for {
		var hdr fcgiHeader
		if err := binary.Read(r, binary.BigEndian, &hdr); err != nil {
			return nil, fmt.Errorf("fcgi read header: %w", err)
		}
		contentLen := int(hdr.ContentLenB1)<<8 | int(hdr.ContentLenB0)
		total := contentLen + int(hdr.PaddingLength)
		buf := make([]byte, total)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("fcgi read body: %w", err)
		}
		content := buf[:contentLen]
		switch hdr.Type {
		case fcgiStdout:
			resp.stdout = append(resp.stdout, content...)
		case fcgiStderr:
			resp.stderr = append(resp.stderr, content...)
		case fcgiEndRequest:
			return &resp, nil
		}
	}
}
