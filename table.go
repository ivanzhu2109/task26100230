package campbroadcast

// u64i32Table 是 uint64 -> int32 的开放寻址哈希表，
// 用于存放组广播 2/3/4 元组的包含-排除计数。
// key 0 保留为空槽标记，合法 key 必须非零（元组 key 的高位带有非零层级标签）。
//
// 相比 map[uint64]int32，该实现把每条目开销压缩到约 12 字节（含负载因子冗余），
// 以满足 192 MiB 的 HeapAlloc 上限。
type u64i32Table struct {
	keys []uint64
	vals []int32
	size int
	mask uint64
}

const tableLoadNum = 3 // size*4 < cap*3，即负载因子 0.75

func (t *u64i32Table) add(key uint64, delta int32) {
	if t.keys == nil {
		t.grow(64)
	} else if (t.size+1)*4 >= len(t.keys)*tableLoadNum {
		t.grow(len(t.keys) * 2)
	}
	i := t.probe(key)
	if t.keys[i] == 0 {
		t.keys[i] = key
		t.size++
	}
	t.vals[i] += delta
}

func (t *u64i32Table) get(key uint64) int32 {
	if t.keys == nil {
		return 0
	}
	return t.vals[t.probe(key)]
}

func (t *u64i32Table) probe(key uint64) uint64 {
	i := mix64(key) & t.mask
	for t.keys[i] != 0 && t.keys[i] != key {
		i = (i + 1) & t.mask
	}
	return i
}

func (t *u64i32Table) grow(capacity int) {
	keys := make([]uint64, capacity)
	vals := make([]int32, capacity)
	mask := uint64(capacity - 1)
	for i, k := range t.keys {
		if k == 0 {
			continue
		}
		j := mix64(k) & mask
		for keys[j] != 0 {
			j = (j + 1) & mask
		}
		keys[j] = k
		vals[j] = t.vals[i]
	}
	t.keys = keys
	t.vals = vals
	t.mask = mask
}

func mix64(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}
