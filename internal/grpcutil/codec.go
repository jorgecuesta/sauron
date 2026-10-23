package grpcutil

import "fmt"

// BytesCodec passes gRPC messages through as raw bytes ([]byte or *[]byte to
// send, *[]byte to receive), for calls
// that only need to know whether a node answers, not what it answered. Its
// name is "proto" so the wire content-subtype stays standard. Install it per
// call or connection (ForceCodec / ForceServerCodec), never globally.
type BytesCodec struct{}

// Marshal returns v's bytes.
func (BytesCodec) Marshal(v any) ([]byte, error) {
	switch b := v.(type) {
	case []byte:
		return b, nil
	case *[]byte:
		return *b, nil
	}
	return nil, fmt.Errorf("BytesCodec: cannot marshal %T, want []byte or *[]byte", v)
}

// Unmarshal stores a copy of data in v.
func (BytesCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("BytesCodec: cannot unmarshal into %T, want *[]byte", v)
	}
	*b = append((*b)[:0], data...)
	return nil
}

// Name is the wire content-subtype.
func (BytesCodec) Name() string { return "proto" }
