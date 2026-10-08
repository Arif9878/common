package requestid_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Arif9878/common/go/requestid"
)

func TestContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := requestid.FromContext(ctx); ok {
		t.Fatal("empty context reported an ID")
	}
	if _, ok := requestid.FromContext(requestid.NewContext(ctx, "")); ok {
		t.Fatal("empty ID reported as present")
	}

	ctx = requestid.NewContext(ctx, "abc")
	if id, ok := requestid.FromContext(ctx); !ok || id != "abc" {
		t.Fatalf("FromContext = %q, %v", id, ok)
	}
}

func TestNew(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id := requestid.New()
		if !requestid.Valid(id) {
			t.Fatalf("New() = %q is not Valid", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = true
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"0f8fad5b-d9cb-469f-a165-70867728950e", true},
		{"a", true},
		{strings.Repeat("a", requestid.MaxLen), true},
		{strings.Repeat("a", requestid.MaxLen+1), false},
		{"", false},
		{"has space", false},
		{"line\nbreak", false},
		{"tab\t", false},
		{"nul\x00", false},
		{"del\x7f", false},
		{"ünïcode", false},
	}
	for _, tt := range tests {
		if got := requestid.Valid(tt.id); got != tt.want {
			t.Errorf("Valid(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}
