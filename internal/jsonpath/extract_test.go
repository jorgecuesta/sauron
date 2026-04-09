package jsonpath

import (
	"errors"
	"testing"
)

func TestExtract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		json    string
		path    string
		want    any
		wantErr error
	}{
		// Basic field access.
		{
			name: "simple field",
			json: `{"height": 42}`,
			path: ".height",
			want: float64(42),
		},
		{
			name: "nested field",
			json: `{"result": {"sync_info": {"latest_block_height": "12345"}}}`,
			path: ".result.sync_info.latest_block_height",
			want: "12345",
		},
		{
			name: "deeply nested",
			json: `{"data":{"bestChain":[{"protocolState":{"consensusState":{"blockHeight":100}}}]}}`,
			path: ".data.bestChain[0].protocolState.consensusState.blockHeight",
			want: float64(100),
		},
		{
			name: "root path",
			json: `42`,
			path: ".",
			want: float64(42),
		},
		{
			name: "empty path returns root",
			json: `{"a":1}`,
			path: "",
			want: map[string]any{"a": float64(1)},
		},

		// Array access.
		{
			name: "array first element",
			json: `{"items": [10, 20, 30]}`,
			path: ".items[0]",
			want: float64(10),
		},
		{
			name: "array last element",
			json: `{"items": [10, 20, 30]}`,
			path: ".items[2]",
			want: float64(30),
		},
		{
			name: "array of objects",
			json: `{"blocks": [{"height": 1}, {"height": 2}]}`,
			path: ".blocks[1].height",
			want: float64(2),
		},

		// Null values.
		{
			name: "null value",
			json: `{"result": null}`,
			path: ".result",
			want: nil,
		},

		// Errors.
		{
			name:    "key not found",
			json:    `{"a": 1}`,
			path:    ".b",
			wantErr: ErrNotFound,
		},
		{
			name:    "array index out of range",
			json:    `{"a": [1]}`,
			path:    ".a[5]",
			wantErr: ErrNotFound,
		},
		{
			name:    "field on non-object",
			json:    `{"a": 42}`,
			path:    ".a.b",
			wantErr: ErrNotFound,
		},
		{
			name:    "index on non-array",
			json:    `{"a": 42}`,
			path:    ".a[0]",
			wantErr: ErrNotFound,
		},
		// Edge cases.
		{
			name: "empty object",
			json: `{}`,
			path: ".",
			want: map[string]any{},
		},
		{
			name: "empty array",
			json: `{"items": []}`,
			path: ".items",
			want: []any{},
		},
		{
			name: "unicode key",
			json: `{"高さ": 42}`,
			path: ".高さ",
			want: float64(42),
		},
		{
			name: "string value",
			json: `{"name": "test"}`,
			path: ".name",
			want: "test",
		},
		{
			name: "boolean value",
			json: `{"ok": true}`,
			path: ".ok",
			want: true,
		},
		{
			name: "nested array then field",
			json: `[{"a": 1}]`,
			path: ".[0].a",
			want: float64(1),
		},

		// More error cases.
		{
			name:    "negative array index",
			json:    `{"a": [1, 2]}`,
			path:    ".a[-1]",
			wantErr: ErrInvalidPath,
		},
		{
			name:    "array index on empty array",
			json:    `{"a": []}`,
			path:    ".a[0]",
			wantErr: ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Extract([]byte(tt.json), tt.path)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil", tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !jsonEqual(got, tt.want) {
				t.Fatalf("got %v (%T), want %v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestExtract_InvalidJSON(t *testing.T) {
	t.Parallel()
	_, err := Extract([]byte("not json"), ".a")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestExtract_EmptyBytes(t *testing.T) {
	t.Parallel()
	_, err := Extract([]byte(""), ".a")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestExtractInt64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		json       string
		path       string
		want       int64
		wantErr    error
		wantAnyErr bool // expect any error, regardless of type
	}{
		// Integer values.
		{
			name: "integer",
			json: `{"height": 20000000}`,
			path: ".height",
			want: 20000000,
		},
		{
			name: "zero",
			json: `{"height": 0}`,
			path: ".height",
			want: 0,
		},
		{
			name: "negative",
			json: `{"height": -1}`,
			path: ".height",
			want: -1,
		},
		{
			name: "large number",
			json: `{"height": 9007199254740992}`,
			path: ".height",
			want: 9007199254740992,
		},

		// String-encoded integers.
		{
			name: "string integer",
			json: `{"height": "12345"}`,
			path: ".height",
			want: 12345,
		},
		{
			name: "string with whitespace",
			json: `{"height": " 42 "}`,
			path: ".height",
			want: 42,
		},

		// Hex strings (EVM).
		{
			name: "hex string lowercase",
			json: `{"result": "0x1312d00"}`,
			path: ".result",
			want: 20000000,
		},
		{
			name: "hex string uppercase prefix",
			json: `{"result": "0X1312D00"}`,
			path: ".result",
			want: 20000000,
		},
		{
			name: "hex string zero",
			json: `{"result": "0x0"}`,
			path: ".result",
			want: 0,
		},

		// Float that is whole.
		{
			name: "whole float",
			json: `{"height": 100.0}`,
			path: ".height",
			want: 100,
		},

		// Nested (Cosmos RPC example).
		{
			name: "cosmos rpc height",
			json: `{"result":{"sync_info":{"latest_block_height":"58272941"}}}`,
			path: ".result.sync_info.latest_block_height",
			want: 58272941,
		},

		// JSON-RPC result (EVM).
		{
			name: "evm block number",
			json: `{"jsonrpc":"2.0","id":1,"result":"0x1312d00"}`,
			path: ".result",
			want: 20000000,
		},

		// GraphQL (Mina example).
		{
			name: "graphql nested",
			json: `{"data":{"bestChain":[{"protocolState":{"consensusState":{"blockHeight":"395000"}}}]}}`,
			path: ".data.bestChain[0].protocolState.consensusState.blockHeight",
			want: 395000,
		},

		// Solana (integer directly in result).
		{
			name: "solana slot",
			json: `{"jsonrpc":"2.0","id":1,"result":300000000}`,
			path: ".result",
			want: 300000000,
		},

		// Error cases.
		{
			name:    "null value",
			json:    `{"height": null}`,
			path:    ".height",
			wantErr: ErrType,
		},
		{
			name:    "boolean value",
			json:    `{"height": true}`,
			path:    ".height",
			wantErr: ErrType,
		},
		{
			name:    "non-integer float",
			json:    `{"height": 1.5}`,
			path:    ".height",
			wantErr: ErrType,
		},
		{
			name:    "object value",
			json:    `{"height": {"nested": 1}}`,
			path:    ".height",
			wantErr: ErrType,
		},
		{
			name:    "array value",
			json:    `{"height": [1, 2]}`,
			path:    ".height",
			wantErr: ErrType,
		},
		{
			name:    "invalid hex",
			json:    `{"result": "0xZZZ"}`,
			path:    ".result",
			wantErr: ErrType,
		},
		{
			name:    "non-numeric string",
			json:    `{"height": "abc"}`,
			path:    ".height",
			wantErr: ErrType,
		},
		{
			name:    "empty string",
			json:    `{"height": ""}`,
			path:    ".height",
			wantErr: ErrType,
		},
		{
			name:    "path not found",
			json:    `{"a": 1}`,
			path:    ".b",
			wantErr: ErrNotFound,
		},

		// Edge cases.
		{
			name:       "invalid JSON",
			json:       `{broken`,
			path:       ".height",
			wantAnyErr: true,
		},
		{
			name:    "invalid path no dot",
			json:    `{"height": 1}`,
			path:    "height",
			wantErr: ErrInvalidPath,
		},
		{
			name:    "hex overflow",
			json:    `{"result": "0xFFFFFFFFFFFFFFFFFF"}`,
			path:    ".result",
			wantErr: ErrType,
		},
		{
			name:    "string integer overflow",
			json:    `{"height": "99999999999999999999"}`,
			path:    ".height",
			wantErr: ErrType,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExtractInt64([]byte(tt.json), tt.path)

			if tt.wantAnyErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error wrapping %v, got nil", tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParsePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		wantErr error
	}{
		{name: "no leading dot", path: "height", wantErr: ErrInvalidPath},
		{name: "unclosed bracket", path: ".[0", wantErr: ErrInvalidPath},
		{name: "non-numeric index", path: ".[abc]", wantErr: ErrInvalidPath},
		{name: "double dot", path: ".a..b", wantErr: ErrInvalidPath},
		{name: "trailing dot", path: ".a.", wantErr: ErrInvalidPath},
		{name: "negative index", path: ".a[-1]", wantErr: ErrInvalidPath},
		{name: "empty bracket", path: ".a[]", wantErr: ErrInvalidPath},
		{name: "float index", path: ".a[1.5]", wantErr: ErrInvalidPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parsePath(tt.path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

// jsonEqual compares two any values, handling maps specially.
func jsonEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		if len(am) != len(bm) {
			return false
		}
		for k, v := range am {
			if !jsonEqual(v, bm[k]) {
				return false
			}
		}
		return true
	}

	aa, aok := a.([]any)
	ba, bok := b.([]any)
	if aok && bok {
		if len(aa) != len(ba) {
			return false
		}
		for i := range aa {
			if !jsonEqual(aa[i], ba[i]) {
				return false
			}
		}
		return true
	}

	return a == b
}
