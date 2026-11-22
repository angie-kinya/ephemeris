package hub

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestSlowViewerIsDroppedNotWaitedFor(t *testing.T) {
	h := New(64)
	fast, slow := h.Subscribe(100), h.Subscribe(1)
	for range 10 {
		h.Write([]byte("x")) // must never block, whatever slow does
	}
	if _, open := <-slow.C(); !open {
		t.Fatal("slow viewer should still hold its first chunk")
	}
	if _, open := <-slow.C(); open {
		t.Fatal("slow viewer should have been dropped")
	}
	if got := len(fast.C()); got != 10 {
		t.Fatalf("fast viewer got %d chunks, want 10", got)
	}
	h.Close()
}

func TestLateJoinerGetsReplay(t *testing.T) {
	h := New(4)
	h.Write([]byte("hello"))
	if got := string(<-h.Subscribe(1).C()); got != "ello" {
		t.Fatalf("replay = %q, want last 4 bytes", got)
	}
	h.Close()
}
