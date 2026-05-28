package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

const (
	fcgiVersion       = 1
	fcgiBeginRequest  = 1
	fcgiEndRequest    = 3
	fcgiParams        = 4
	fcgiStdin         = 5
	fcgiStdout        = 6
	fcgiStderr        = 7
	fcgiResponderRole = 1
	fcgiRequestID     = 1
	fcgiMaxRecordBody = 65535
)

type fcgiResponse struct {
	stdout []byte
	stderr []byte
}

func writeFCGIRequest(w io.Writer, env map[string]string, body []byte) error {
	beginBody := []byte{
		0, fcgiResponderRole,
		0, 
		0, 0, 0, 0, 0,
	}
	if err := writeFCGIRecord(w, fcgiBeginRequest, beginBody); err != nil {
		return err
	}
	if err := writeFCGIParams(w, env); err != nil {
		return err
	}
	if err := writeFCGIRecord(w, fcgiParams, nil); err != nil {
		return err
	}
	for len(body) > 0 {
		chunkLen := len(body)
		if chunkLen > fcgiMaxRecordBody {
			chunkLen = fcgiMaxRecordBody
		}
		if err := writeFCGIRecord(w, fcgiStdin, body[:chunkLen]); err != nil {
			return err
		}
		body = body[chunkLen:]
	}
	return writeFCGIRecord(w, fcgiStdin, nil)
}

func writeFCGIParams(w io.Writer, env map[string]string) error {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		if err := writeFCGIRecord(w, fcgiParams, buf.Bytes()); err != nil {
			return err
		}
		buf.Reset()
		return nil
	}
	for _, key := range keys {
		encoded := encodeFCGINameValue(key, env[key])
		if buf.Len()+len(encoded) > fcgiMaxRecordBody {
			if err := flush(); err != nil {
				return err
			}
		}
		buf.Write(encoded)
	}
	return flush()
}

func readFCGIResponse(r io.Reader) (*fcgiResponse, error) {
	resp := &fcgiResponse{}
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF {
				return resp, nil
			}
			return nil, fmt.Errorf("reading fastcgi header: %w", err)
		}
		if header[0] != fcgiVersion {
			return nil, fmt.Errorf("unsupported fastcgi version %d", header[0])
		}
		requestID := binary.BigEndian.Uint16(header[2:4])
		if requestID != fcgiRequestID {
			return nil, fmt.Errorf("unexpected fastcgi request id %d", requestID)
		}
		contentLength := int(binary.BigEndian.Uint16(header[4:6]))
		paddingLength := int(header[6])
		content := make([]byte, contentLength)
		if contentLength > 0 {
			if _, err := io.ReadFull(r, content); err != nil {
				return nil, fmt.Errorf("reading fastcgi content: %w", err)
			}
		}
		if paddingLength > 0 {
			if _, err := io.CopyN(io.Discard, r, int64(paddingLength)); err != nil {
				return nil, fmt.Errorf("reading fastcgi padding: %w", err)
			}
		}

		switch header[1] {
		case fcgiStdout:
			resp.stdout = append(resp.stdout, content...)
		case fcgiStderr:
			resp.stderr = append(resp.stderr, content...)
		case fcgiEndRequest:
			return resp, nil
		default:
		}
	}
}

func writeFCGIRecord(w io.Writer, recordType uint8, body []byte) error {
	for len(body) > fcgiMaxRecordBody {
		if err := writeFCGIRecord(w, recordType, body[:fcgiMaxRecordBody]); err != nil {
			return err
		}
		body = body[fcgiMaxRecordBody:]
	}
	header := []byte{
		fcgiVersion,
		recordType,
		0, fcgiRequestID,
		byte(len(body) >> 8), byte(len(body)),
		0, 0,
	}
	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("writing fastcgi header: %w", err)
	}
	if len(body) == 0 {
		return nil
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("writing fastcgi content: %w", err)
	}
	return nil
}

func encodeFCGINameValue(name, value string) []byte {
	var out []byte
	out = appendFCGILength(out, len(name))
	out = appendFCGILength(out, len(value))
	out = append(out, name...)
	out = append(out, value...)
	return out
}

func appendFCGILength(out []byte, length int) []byte {
	if length < 128 {
		return append(out, byte(length))
	}
	return append(out,
		byte(length>>24)|0x80,
		byte(length>>16),
		byte(length>>8),
		byte(length),
	)
}
