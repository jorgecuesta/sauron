// Package jsonpath extracts values from JSON documents using dot-notation paths.
//
// Supported path syntax:
//
//	.key              — object field lookup
//	.key1.key2        — nested object lookup
//	.arr[0]           — array index
//	.arr[0].field     — array index then field
//
// Paths must start with ".". An empty path returns the root value.
package jsonpath

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	// ErrInvalidPath indicates the path string is malformed.
	ErrInvalidPath = errors.New("jsonpath: invalid path")
	// ErrNotFound indicates the path does not exist in the document.
	ErrNotFound = errors.New("jsonpath: not found")
	// ErrType indicates the value at the path cannot be converted to the requested type.
	ErrType = errors.New("jsonpath: type error")
)

// Extract traverses raw JSON bytes using the given dot-notation path and returns
// the value at that path as an any (json-decoded).
func Extract(data []byte, path string) (any, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("jsonpath: invalid JSON: %w", err)
	}

	if path == "" || path == "." {
		return root, nil
	}

	segments, err := parsePath(path)
	if err != nil {
		return nil, err
	}

	current := root
	for _, seg := range segments {
		switch s := seg.(type) {
		case string:
			m, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: expected object at %q", ErrNotFound, s)
			}
			val, exists := m[s]
			if !exists {
				return nil, fmt.Errorf("%w: key %q not found", ErrNotFound, s)
			}
			current = val
		case int:
			arr, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("%w: expected array for index %d", ErrNotFound, s)
			}
			if s < 0 || s >= len(arr) {
				return nil, fmt.Errorf("%w: index %d out of range (len %d)", ErrNotFound, s, len(arr))
			}
			current = arr[s]
		}
	}

	return current, nil
}

// ExtractInt64 extracts a value and converts it to int64.
// Supports: integer numbers, string-encoded integers, hex strings (0x prefix),
// and float numbers that are whole values.
func ExtractInt64(data []byte, path string) (int64, error) {
	val, err := Extract(data, path)
	if err != nil {
		return 0, err
	}

	return toInt64(val)
}

func toInt64(val any) (int64, error) {
	switch v := val.(type) {
	case nil:
		return 0, fmt.Errorf("%w: null value", ErrType)
	case bool:
		return 0, fmt.Errorf("%w: boolean value", ErrType)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("%w: non-finite float", ErrType)
		}
		if v != math.Trunc(v) {
			return 0, fmt.Errorf("%w: non-integer float %v", ErrType, v)
		}
		if v > math.MaxInt64 || v < math.MinInt64 {
			return 0, fmt.Errorf("%w: overflow %v", ErrType, v)
		}
		return int64(v), nil
	case string:
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "0x") || strings.HasPrefix(v, "0X") {
			n, err := strconv.ParseInt(v[2:], 16, 64)
			if err != nil {
				return 0, fmt.Errorf("%w: invalid hex %q: %v", ErrType, v, err)
			}
			return n, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: cannot parse %q as int64: %v", ErrType, v, err)
		}
		return n, nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrType, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("%w: unsupported type %T", ErrType, val)
	}
}

// segment is either a string (object key) or int (array index).
type segment = any

func parsePath(path string) ([]segment, error) {
	if !strings.HasPrefix(path, ".") {
		return nil, fmt.Errorf("%w: path must start with '.'", ErrInvalidPath)
	}

	path = path[1:] // strip leading dot
	if path == "" {
		return nil, nil
	}

	var segments []segment
	for path != "" {
		// Check for array index.
		if path[0] == '[' {
			end := strings.IndexByte(path, ']')
			if end < 0 {
				return nil, fmt.Errorf("%w: unclosed bracket", ErrInvalidPath)
			}
			idxStr := path[1:end]
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid array index %q", ErrInvalidPath, idxStr)
			}
			if idx < 0 {
				return nil, fmt.Errorf("%w: negative array index %d", ErrInvalidPath, idx)
			}
			segments = append(segments, idx)
			path = path[end+1:]
			if path != "" && path[0] == '.' {
				path = path[1:]
				if path == "" {
					return nil, fmt.Errorf("%w: trailing dot", ErrInvalidPath)
				}
			}
			continue
		}

		// Find next separator (dot or bracket).
		end := len(path)
		for i := 0; i < len(path); i++ {
			if path[i] == '.' || path[i] == '[' {
				end = i
				break
			}
		}
		key := path[:end]
		if key == "" {
			return nil, fmt.Errorf("%w: empty key", ErrInvalidPath)
		}
		segments = append(segments, key)
		path = path[end:]
		if path != "" && path[0] == '.' {
			path = path[1:]
			if path == "" {
				return nil, fmt.Errorf("%w: trailing dot", ErrInvalidPath)
			}
		}
	}
	return segments, nil
}
