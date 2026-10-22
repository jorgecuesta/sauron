package grpcutil

import (
	"testing"

	"google.golang.org/grpc/codes"
)

func TestIsNodeFailure(t *testing.T) {
	t.Parallel()
	failures := map[codes.Code]bool{codes.Internal: true, codes.Unavailable: true, codes.DataLoss: true, codes.Unknown: true}
	for c := codes.OK; c <= codes.Unauthenticated; c++ {
		if got := IsNodeFailure(c); got != failures[c] {
			t.Errorf("IsNodeFailure(%s) = %v, want %v", c, got, failures[c])
		}
	}
	if IsNodeFailure(codes.Code(99)) {
		t.Error("IsNodeFailure(99) = true for an unknown code")
	}
}

func TestParseCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		want   codes.Code
		wantOK bool
	}{
		{name: "unavailable", want: codes.Unavailable, wantOK: true},
		{name: "UNAVAILABLE", want: codes.Unavailable, wantOK: true},
		{name: "resource_exhausted", want: codes.ResourceExhausted, wantOK: true},
		{name: "ResourceExhausted", want: codes.ResourceExhausted, wantOK: true},
		{name: "RESOURCE-EXHAUSTED", want: codes.ResourceExhausted, wantOK: true},
		{name: "  deadline_exceeded  ", want: codes.DeadlineExceeded, wantOK: true},
		{name: "ok", want: codes.OK, wantOK: true},
		{name: "unauthenticated", want: codes.Unauthenticated, wantOK: true},
		{name: "", wantOK: false},
		{name: "unavailible", wantOK: false},
		{name: "14", wantOK: false},
		{name: "unavailable_", want: codes.Unavailable, wantOK: true},
		{name: "ünavailable", wantOK: false},
	}
	for _, tt := range tests {
		got, ok := ParseCode(tt.name)
		if ok != tt.wantOK || (ok && got != tt.want) {
			t.Errorf("ParseCode(%q) = %s, %v; want %s, %v", tt.name, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestIsBreakerFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		configured []string
		code       codes.Code
		want       bool
	}{
		{name: "default set when unconfigured", configured: nil, code: codes.Unavailable, want: true},
		{name: "default set excludes NotFound", configured: nil, code: codes.NotFound, want: false},
		{name: "empty list is the default", configured: []string{}, code: codes.Internal, want: true},
		{name: "configured set replaces default: listed", configured: []string{"resource_exhausted"}, code: codes.ResourceExhausted, want: true},
		{name: "configured set replaces default: unlisted default", configured: []string{"resource_exhausted"}, code: codes.Unavailable, want: false},
		{name: "configured name forms", configured: []string{"DEADLINE_EXCEEDED"}, code: codes.DeadlineExceeded, want: true},
		{name: "OK never counts", configured: []string{"ok"}, code: codes.OK, want: false},
	}
	for _, tt := range tests {
		if got := IsBreakerFailure(tt.configured, tt.code); got != tt.want {
			t.Errorf("%s: IsBreakerFailure(%v, %s) = %v, want %v", tt.name, tt.configured, tt.code, got, tt.want)
		}
	}
}
