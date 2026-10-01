package campbroadcast

import (
	"math/rand"
	"testing"
)

func TestTableBasic(t *testing.T) {
	var tab u64i32Table
	ref := make(map[uint64]int32)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200000; i++ {
		// key 高位带非零标签，模拟元组 key
		k := uint64(rng.Intn(5000)) | (1 << 60)
		d := int32(rng.Intn(3) + 1)
		tab.add(k, d)
		ref[k] += d
	}
	for k, v := range ref {
		if got := tab.get(k); got != v {
			t.Fatalf("key %d: got %d want %d", k, got, v)
		}
	}
	if got := tab.get(7 << 60); got != 0 {
		t.Fatalf("missing key: got %d want 0", got)
	}
}
