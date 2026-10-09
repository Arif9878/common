package requestid_test

import (
	"context"
	"testing"

	"github.com/Arif9878/common/go/requestid"
)

// FuzzValid checks that every accepted ID is safe to put in a log line or
// a header: bounded, printable ASCII without spaces, and kept intact
// through the context.
func FuzzValid(f *testing.F) {
	f.Add("req-3f2a")
	f.Add("with space")
	f.Add("line\nbreak")
	f.Add("ünïcode")
	f.Fuzz(func(t *testing.T, id string) {
		if !requestid.Valid(id) {
			return
		}
		if len(id) == 0 || len(id) > requestid.MaxLen {
			t.Fatalf("accepted length %d", len(id))
		}
		for i := 0; i < len(id); i++ {
			if id[i] <= ' ' || id[i] > '~' {
				t.Fatalf("accepted byte %q in %q", id[i], id)
			}
		}
		if got, ok := requestid.FromContext(requestid.NewContext(context.Background(), id)); !ok || got != id {
			t.Fatalf("round trip: %q %v", got, ok)
		}
	})
}
