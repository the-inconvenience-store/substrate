// Package jsonx centralizes Substrate's JSON codec. It wraps bytedance/sonic in
// encoding/json-compatible mode (sonic.ConfigStd: HTML escaping + sorted map keys),
// so marshaled output is byte-for-byte equivalent to encoding/json while decoding
// allocates far less — the hot path is unmarshaling stored state into map[string]any
// on every record read, list row, and history event.
//
// Centralizing the codec here keeps the dependency in one place and makes it
// trivial to retune (e.g. switch to sonic.ConfigDefault) or swap out.
package jsonx

import (
	"io"

	"github.com/bytedance/sonic"
)

// std behaves like encoding/json (escapes HTML, sorts map keys) but uses sonic's
// codec underneath.
var std = sonic.ConfigStd

// Marshal is a drop-in replacement for encoding/json.Marshal.
func Marshal(v any) ([]byte, error) { return std.Marshal(v) }

// Unmarshal is a drop-in replacement for encoding/json.Unmarshal.
func Unmarshal(data []byte, v any) error { return std.Unmarshal(data, v) }

// NewEncoder is a drop-in replacement for encoding/json.NewEncoder.
func NewEncoder(w io.Writer) sonic.Encoder { return std.NewEncoder(w) }

// NewDecoder is a drop-in replacement for encoding/json.NewDecoder.
func NewDecoder(r io.Reader) sonic.Decoder { return std.NewDecoder(r) }
