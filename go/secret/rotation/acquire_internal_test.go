package rotation

import (
	"testing"
	"time"
)

// TestAcquireNeverReturnsRetiredEntry simulates the interleaving a stress
// test can rarely hit: an acquirer sees the old entry after the rotator has
// retired it. Acquire must move on to the replacement instead of taking a
// use on a resource that is about to be closed.
func TestAcquireNeverReturnsRetiredEntry(t *testing.T) {
	r := &Rotator[string]{}
	old := &entry[string]{res: "old", drained: make(chan struct{})}
	old.retire()
	r.cur.Store(old)

	got := make(chan string, 1)
	go func() {
		res, release, err := r.Acquire()
		if err != nil {
			t.Error(err)
		}
		release()
		got <- res
	}()

	select {
	case res := <-got:
		t.Fatalf("Acquire returned %q from a retired entry", res)
	case <-time.After(50 * time.Millisecond):
	}
	r.cur.Store(&entry[string]{res: "new", drained: make(chan struct{})})
	if res := <-got; res != "new" {
		t.Fatalf("Acquire returned %q, want the replacement", res)
	}
	if old.refs.Load() != retiredBit {
		t.Errorf("retired entry gained uses: refs = %#x", old.refs.Load())
	}
}
