package campbroadcast

import (
	"math/rand"
	"runtime"
	"testing"
	"time"
)

// 规模验证：所有维度同时达到题目上限。
//
//	10 万营员 / 20 万条营员-组关系 / 4096 组 / 每营员 2 组
//	2 万监护人 / 16 万条当前关联（每人 8 营员，并集 <=16 组）
//	100 万条消息 / 400 万条消息-组关系（每条 4 组，热门组高度重叠 + 稀疏组）
//	100 万对成功已读（含换绑后不可见的记录）
//	1.2 万次换绑+计数交替，抽样用逐消息参考实现核对
//	全程保留完整初始化输入，GC 后 HeapAlloc <= 192 MiB
func TestScale(t *testing.T) {
	const (
		nCampers   = 100_000
		nGroups    = 4096
		nHot       = 64
		nGuardians = 20_000
		nMessages  = 1_000_000
		readsPerG  = 50 // 20k * 50 = 1M 对已读
		nRounds    = 12_000
		heapLimit  = 192 << 20
	)
	rng := rand.New(rand.NewSource(26100230))

	t0 := time.Now()
	// ---- 生成并保留完整初始化输入 ----
	camperGroups := make(map[uint32][]uint32, nCampers)
	for c := uint32(0); c < nCampers; c++ {
		camperGroups[c] = []uint32{
			uint32(rng.Intn(nHot)),                // 热门组
			uint32(nHot + rng.Intn(nGroups-nHot)), // 稀疏组
		}
	}
	guardians := make([]uint32, nGuardians)
	for i := range guardians {
		guardians[i] = uint32(1_000_000 + i)
	}
	links := make(map[uint32][]uint32, nGuardians)
	for _, g := range guardians {
		seen := map[uint32]bool{}
		for len(seen) < 8 {
			c := uint32(rng.Intn(nCampers))
			if !seen[c] {
				seen[c] = true
				links[g] = append(links[g], c)
			}
		}
	}
	t.Logf("生成初始化输入: %v (营员-组关系 %d 条)", time.Since(t0), 2*nCampers)

	t1 := time.Now()
	r := New(camperGroups, guardians, links)
	t.Logf("初始化组件: %v (当前关联 %d 条)", time.Since(t1), 8*nGuardians)

	// ---- 发布 100 万条组广播：85% 纯热门组（高度重叠），15% 热门+稀疏混合 ----
	t2 := time.Now()
	published := make([][4]uint32, 0, nMessages) // 参考实现用的路由副本
	for i := 0; i < nMessages; i++ {
		var gs [4]uint32
		if rng.Intn(100) < 85 {
			seen := map[uint32]bool{}
			k := 0
			for k < 4 {
				g := uint32(rng.Intn(nHot))
				if !seen[g] {
					seen[g] = true
					gs[k] = g
					k++
				}
			}
		} else {
			gs[0] = uint32(rng.Intn(nHot))
			seen := map[uint32]bool{gs[0]: true}
			k := 1
			for k < 4 {
				g := uint32(nHot + rng.Intn(nGroups-nHot))
				if !seen[g] {
					seen[g] = true
					gs[k] = g
					k++
				}
			}
		}
		id := r.Publish(Route{Kind: RouteGroups, Groups: gs[:]})
		if int(id) != i+1 {
			t.Fatalf("message id = %d, want %d", id, i+1)
		}
		published = append(published, gs)
	}
	t.Logf("发布 %d 条消息（%d 条消息-组关系）: %v", nMessages, 4*nMessages, time.Since(t2))
	t.Logf("元组计数表: %d 条目 / 容量 %d", r.tupleCnt.size, len(r.tupleCnt.keys))

	// ---- 100 万对成功已读 ----
	t3 := time.Now()
	refRead := make(map[uint32]map[uint32]struct{}, nGuardians)
	totalReads := 0
	for _, g := range guardians {
		rs := map[uint32]struct{}{}
		for len(rs) < readsPerG {
			m := uint32(1 + rng.Intn(nMessages))
			if r.MarkRead(g, m) {
				rs[m] = struct{}{}
			}
		}
		refRead[g] = rs
		totalReads += len(rs)
	}
	if totalReads != nGuardians*readsPerG {
		t.Fatalf("successful reads = %d, want %d", totalReads, nGuardians*readsPerG)
	}
	t.Logf("标记已读 %d 对: %v", totalReads, time.Since(t3))

	// ---- 参考实现：逐消息暴力计算当前未读 ----
	refLinks := make(map[uint32][]uint32, nGuardians)
	for g, cs := range links {
		refLinks[g] = append([]uint32(nil), cs...)
	}
	var groupOf [nGroups]bool
	naiveUnread := func(g uint32) uint64 {
		for i := range groupOf {
			groupOf[i] = false
		}
		for _, c := range refLinks[g] {
			for _, grp := range camperGroups[c] {
				groupOf[grp] = true
			}
		}
		var visible uint64
		for _, gs := range published {
			for _, grp := range gs {
				if groupOf[grp] {
					visible++
					break
				}
			}
		}
		var readVisible uint64
		for m := range refRead[g] {
			for _, grp := range published[m-1] {
				if groupOf[grp] {
					readVisible++
					break
				}
			}
		}
		return visible - readVisible
	}

	// ---- 1.2 万次换绑 + 计数交替，抽样核对 ----
	t4 := time.Now()
	checked := 0
	for round := 0; round < nRounds; round++ {
		g := guardians[rng.Intn(nGuardians)]
		seen := map[uint32]bool{}
		var campers []uint32
		for len(seen) < 8 {
			c := uint32(rng.Intn(nCampers))
			if !seen[c] {
				seen[c] = true
				campers = append(campers, c)
			}
		}
		r.ReplaceLinks(g, campers)
		refLinks[g] = campers
		got := r.UnreadCount(g)
		if round%60 == 0 {
			if want := naiveUnread(g); got != want {
				t.Fatalf("round %d guardian %d: unread = %d, want %d", round, g, got, want)
			}
			checked++
		}
	}
	t.Logf("换绑+计数交替 %d 次: %v (抽样核对 %d 次全部一致)", nRounds, time.Since(t4), checked)

	// ---- 内存断言：完整状态 + 保留的初始化输入 + 参考数据统一计入 ----
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("GC 后 HeapAlloc = %.1f MiB (上限 192 MiB)", float64(ms.HeapAlloc)/(1<<20))
	if ms.HeapAlloc > heapLimit {
		t.Fatalf("HeapAlloc = %d bytes, exceeds 192 MiB", ms.HeapAlloc)
	}
	t.Logf("总耗时: %v", time.Since(t0))

	// 保证初始化输入与参考数据在内存测量点之前一直存活
	runtime.KeepAlive(r)
	runtime.KeepAlive(camperGroups)
	runtime.KeepAlive(guardians)
	runtime.KeepAlive(links)
	runtime.KeepAlive(published)
	runtime.KeepAlive(refRead)
	runtime.KeepAlive(refLinks)
}
