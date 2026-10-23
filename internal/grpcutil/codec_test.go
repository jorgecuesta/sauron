package grpcutil

import (
	"bytes"
	"testing"
)

func TestBytesCodec(t *testing.T) {
	t.Parallel()
	c := BytesCodec{}
	if c.Name() != "proto" {
		t.Errorf("Name = %q, want proto (the wire content-subtype)", c.Name())
	}

	raw := []byte{0xff, 0x00, 0x03}
	for _, v := range []any{raw, &raw} {
		got, err := c.Marshal(v)
		if err != nil || !bytes.Equal(got, raw) {
			t.Errorf("Marshal(%T) = %v, %v; want %v", v, got, err, raw)
		}
	}
	empty := []byte{}
	if got, err := c.Marshal(&empty); err != nil || len(got) != 0 {
		t.Errorf("Marshal(empty) = %v, %v", got, err)
	}
	if _, err := c.Marshal("not bytes"); err == nil {
		t.Error("Marshal(string) succeeded")
	}

	data := []byte("payload")
	var out []byte
	if err := c.Unmarshal(data, &out); err != nil || string(out) != "payload" {
		t.Fatalf("Unmarshal = %q, %v", out, err)
	}
	data[0] = 'X' // the buffer gRPC passes in may be reused
	if string(out) != "payload" {
		t.Errorf("Unmarshal kept a reference to the input buffer: %q", out)
	}
	if err := c.Unmarshal(data, out); err == nil {
		t.Error("Unmarshal into a non-pointer succeeded")
	}
}
