// Package runtimehost implements the private php_wasm_runtime_v1 ABI for wazero.
// The embedding host supplies configuration and owns event attribution/storage.
package runtimehost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

const (
	ModuleName     = "php_wasm_runtime_v1"
	MaxConfigBytes = 262144
	MaxEventBytes  = 65536
	MaxEvents      = 512
)

// Hook contains PHP statements (without PHP opening/closing tags). They execute
// inside the application's sandbox; see docs/RUNTIME.md for the script contract.
type Hook struct {
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// Config is supplied by the trusted embedding host, never by an HTTP parameter,
// PHP environment variable, application file, or application-accessible API.
type Config struct {
	Version   int             `json:"version"`
	Observe   []string        `json:"observe,omitempty"`
	Hooks     map[string]Hook `json:"hooks,omitempty"`
	Bootstrap string          `json:"bootstrap,omitempty"`
	MaxEvents int             `json:"max_events,omitempty"`
}

func (c Config) Marshal() ([]byte, error) {
	if c.Version != 1 {
		return nil, fmt.Errorf("runtime configuration version must be 1")
	}
	if c.MaxEvents < 0 || c.MaxEvents > MaxEvents {
		return nil, fmt.Errorf("max_events must be 0 (default) through %d", MaxEvents)
	}
	if len(c.Hooks) > 128 || len(c.Observe) > 128 {
		return nil, fmt.Errorf("at most 128 hooks and 128 observed names are supported")
	}
	validName := func(name string) bool {
		return name != "" && len(name) <= 1024 && !strings.ContainsRune(name, 0) && name == strings.ToLower(name)
	}
	for _, name := range c.Observe {
		if !validName(name) {
			return nil, fmt.Errorf("invalid observed function name %q: use lowercase function or class::method", name)
		}
	}
	for name := range c.Hooks {
		if !validName(name) {
			return nil, fmt.Errorf("invalid hook name %q: use lowercase function or class::method", name)
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("runtime configuration exceeds %d bytes", MaxConfigBytes)
	}
	if bytes.Contains(data, []byte(`\u0000`)) {
		return nil, fmt.Errorf("runtime configuration cannot contain NUL bytes")
	}
	return data, nil
}

func DecodeConfig(r io.Reader) (Config, error) {
	var c Config
	data, err := io.ReadAll(io.LimitReader(r, MaxConfigBytes+1))
	if err != nil {
		return c, err
	}
	if len(data) > MaxConfigBytes {
		return c, fmt.Errorf("runtime configuration is too large")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("expected exactly one configuration object")
	}
	_, err = c.Marshal()
	return c, err
}

// Host callbacks must be safe for concurrent module instances. Config is called
// once per PHP request, including every request in persistent FastCGI mode.
// Module identity and request correlation should be assigned here by the host.
// PHP objects, paths, result strings, and observations are untrusted evidence.
type Host struct {
	Config   func(context.Context, api.Module) (Config, error)
	Emit     func(context.Context, api.Module, json.RawMessage) error
	Database func(context.Context, api.Module, json.RawMessage) ([]byte, error)
	Error    func(context.Context, error)
}

func (h Host) Register(ctx context.Context, runtime wazero.Runtime) error {
	if h.Config == nil || h.Emit == nil {
		return fmt.Errorf("runtime host requires Config and Emit callbacks")
	}
	report := func(ctx context.Context, err error) int32 {
		if h.Error != nil {
			h.Error(ctx, err)
		}
		return -1
	}
	_, err := runtime.NewHostModuleBuilder(ModuleName).
		NewFunctionBuilder().WithFunc(func(ctx context.Context, module api.Module, pointer, capacity uint32) int32 {
		if capacity != MaxConfigBytes {
			return report(ctx, fmt.Errorf("invalid runtime configuration buffer size"))
		}
		config, err := h.Config(ctx, module)
		if err != nil {
			return report(ctx, err)
		}
		data, err := config.Marshal()
		if err != nil {
			return report(ctx, err)
		}
		if !module.Memory().Write(pointer, data) {
			return report(ctx, fmt.Errorf("runtime configuration buffer is outside guest memory"))
		}
		return int32(len(data))
	}).Export("config").
		NewFunctionBuilder().WithFunc(func(ctx context.Context, module api.Module, pointer, length uint32) int32 {
		if length == 0 || length > MaxEventBytes {
			return report(ctx, fmt.Errorf("invalid runtime event size"))
		}
		data, ok := module.Memory().Read(pointer, length)
		if !ok {
			return report(ctx, fmt.Errorf("runtime event is outside guest memory"))
		}
		var envelope struct {
			Version int    `json:"version"`
			Kind    string `json:"kind"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return report(ctx, err)
		}
		if envelope.Version != 1 || envelope.Kind == "" {
			return report(ctx, fmt.Errorf("invalid runtime event envelope"))
		}
		// Guest memory can change as soon as this import returns.
		if err := h.Emit(ctx, module, append(json.RawMessage(nil), data...)); err != nil {
			return report(ctx, err)
		}
		return 0
	}).Export("emit").
		NewFunctionBuilder().WithFunc(func(ctx context.Context, module api.Module, pointer, length, output, capacity uint32) int32 {
		if h.Database == nil {
			return -1
		}
		if length == 0 || length > 1048576 || capacity != 4194304 {
			return report(ctx, fmt.Errorf("invalid database buffer size"))
		}
		data, ok := module.Memory().Read(pointer, length)
		if !ok {
			return report(ctx, fmt.Errorf("database request outside guest memory"))
		}
		// Validate the complete destination before executing a stateful operation.
		if _, ok := module.Memory().Read(output, capacity); !ok {
			return report(ctx, fmt.Errorf("database response outside guest memory"))
		}
		request := append(json.RawMessage(nil), data...)
		if !json.Valid(request) {
			return report(ctx, fmt.Errorf("invalid database JSON"))
		}
		response, err := h.Database(ctx, module, request)
		if err != nil {
			return report(ctx, err)
		}
		if len(response) == 0 || len(response) > int(capacity) || !json.Valid(response) {
			return report(ctx, fmt.Errorf("invalid database response"))
		}
		if !module.Memory().Write(output, response) {
			return report(ctx, fmt.Errorf("database response outside guest memory"))
		}
		return int32(len(response))
	}).Export("database").Instantiate(ctx)
	return err
}
