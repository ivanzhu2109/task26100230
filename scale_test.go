package broadcast

import (
	"fmt"
	"math/rand"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

// Scale verification: pushes camper count, camper-group relations,
// guardian count, current links, message count and read pairs to their
// documented maxima at the same time, with broadcasts mixing hot
// overlapping groups and sparse groups. Runs 10k alternating
// ReplaceLinks/UnreadCount rounds, spot-checks counts against a
// per-message reference scan, asserts the 192 MiB HeapAlloc budget and
// reports per-phase timings.
func TestScaleVerification(t *testing.T) {
	if testing.Short() {
		t.Skip("scale verification skipped in -short mode")
	}
	rng := rand.New(rand.NewSource(927410))

	const (
		nGroups     = 4096
		nHot        = 128
		nCampers    = 100_000
		groupsPerCp = 2 // -> 200k camper-group relations
		nGuardians  = 20_000
		campersPerG = 8 // -> 160k current guardian-camper links
		totalMsgs   = 1_000_000
		nCombos     = 2048 // distinct 4-group broadcast templates
		readsPerG   = 50   // -> 1M successful guardian-message read pairs
		nRounds     = 10_000
		heapLimit   = 192 << 20
	)
	groupBase := uint32(1_000_000)
	groupID := func(i int) uint32 { return groupBase + uint32(i) }

	// ---- Phase 1: build initialization input ----
	start := time.Now()
	campers := make([]CamperInfo, nCampers)
	for i := range campers {
		campers[i] = CamperInfo{
			Camper: uint32(10_000_000 + i),
			Groups: []uint32{
				groupID(rng.Intn(nHot)),          // hot group
				groupID(nHot + i%(nGroups-nHot)), // sparse group
			},
		}
	}
	guardianIDs := make([]uint32, nGuardians)
	for i := range guardianIDs {
		guardianIDs[i] = uint32(1 + i)
	}
	links := make([]GuardianLink, nGuardians)
	for i := range links {
		set := make([]uint32, campersPerG)
		for j := range set {
			set[j] = campers[rng.Intn(nCampers)].Camper
		}
		links[i] = GuardianLink{Guardian: guardianIDs[i], Campers: set}
	}
	// Broadcast templates: 2 hot + 2 sparse groups each. Hot groups are
	// shared across many templates (heavy overlap); sparse groups are
	// spread so each appears in roughly one template.
	combos := make([][4]uint32, nCombos)
	for i := range combos {
		h1, h2 := rng.Intn(nHot), rng.Intn(nHot)
		for h2 == h1 {
			h2 = rng.Intn(nHot)
		}
		s1 := nHot + (2*i)%(nGroups-nHot)
		s2 := nHot + (2*i+1)%(nGroups-nHot)
		combos[i] = [4]uint32{groupID(h1), groupID(h2), groupID(s1), groupID(s2)}
	}
	// routeOf deterministically derives the route of message seq i.
	routeOf := func(i int) Route {
		switch i % 200 {
		case 0:
			return Route{Kind: RouteAllGuardians}
		case 1:
			return Route{Kind: RouteSingleGuardian, Guardian: guardianIDs[(i/200)%nGuardians]}
		default:
			cb := combos[i%nCombos]
			return Route{Kind: RouteGroups, Groups: cb[:]}
		}
	}
	t.Logf("phase init-input: %v", time.Since(start))

	// ---- Phase 2: component construction ----
	start = time.Now()
	c := New(campers, guardianIDs, links)
	t.Logf("phase construct: %v", time.Since(start))

	// ---- Phase 3: publish 1M messages (3.96M message-group relations)
	// and build a driver-side group->messages index for read generation.
	start = time.Now()
	csr := make([][]uint32, nGroups)
	for i := 0; i < totalMsgs; i++ {
		r := routeOf(i)
		id := c.Publish(r)
		if want := uint32(i + 1); id != want {
			t.Fatalf("publish id = %d, want %d", id, want)
		}
		if r.Kind == RouteGroups {
			for _, g := range r.Groups {
				csr[g-groupBase] = append(csr[g-groupBase], id)
			}
		}
	}
	nRelations := 0
	for _, l := range csr {
		nRelations += len(l)
	}
	t.Logf("phase publish: %v (%d messages, %d message-group relations)", time.Since(start), totalMsgs, nRelations)

	// ---- Phase 4: 1M successful MarkRead pairs ----
	start = time.Now()
	camperGroupsOf := make(map[uint32][]uint32, nCampers)
	for _, ci := range campers {
		camperGroupsOf[ci.Camper] = ci.Groups
	}
	curLinks := make(map[uint32][]uint32, nGuardians)
	for _, l := range links {
		curLinks[l.Guardian] = l.Campers
	}
	readLists := make(map[uint32][]uint32, nGuardians)
	nReads := 0
	for _, g := range guardianIDs {
		// Visible groups of this guardian; pick the sparse one with the
		// smallest message list and read 50 evenly spaced messages.
		best, bestLen := -1, int(^uint(0)>>1)
		for _, cp := range curLinks[g] {
			for _, gid := range camperGroupsOf[cp] {
				idx := int(gid - groupBase)
				if idx >= nHot && len(csr[idx]) < bestLen {
					best, bestLen = idx, len(csr[idx])
				}
			}
		}
		if best < 0 || bestLen < readsPerG {
			t.Fatalf("guardian %d: no sparse group with >= %d messages", g, readsPerG)
		}
		list := csr[best]
		step := bestLen / readsPerG
		for j := 0; j < readsPerG; j++ {
			m := list[j*step]
			if !c.MarkRead(g, m) {
				t.Fatalf("MarkRead(%d, %d) unexpectedly failed", g, m)
			}
			readLists[g] = append(readLists[g], m)
			nReads++
		}
	}
	if nReads != nGuardians*readsPerG {
		t.Fatalf("successful reads = %d, want %d", nReads, nGuardians*readsPerG)
	}
	t.Logf("phase mark-read: %v (%d successful guardian-message pairs)", time.Since(start), nReads)

	// referenceCount scans every message for one guardian.
	referenceCount := func(g uint32) uint64 {
		gset := make(map[uint32]bool, 16)
		for _, cp := range curLinks[g] {
			for _, gid := range camperGroupsOf[cp] {
				gset[gid] = true
			}
		}
		readSet := make(map[uint32]bool, len(readLists[g]))
		for _, m := range readLists[g] {
			readSet[m] = true
		}
		var n uint64
		for i := 0; i < totalMsgs; i++ {
			id := uint32(i + 1)
			if readSet[id] {
				continue
			}
			r := routeOf(i)
			vis := false
			switch r.Kind {
			case RouteAllGuardians:
				vis = true
			case RouteSingleGuardian:
				vis = r.Guardian == g
			default:
				for _, gid := range r.Groups {
					if gset[gid] {
						vis = true
						break
					}
				}
			}
			if vis {
				n++
			}
		}
		return n
	}

	// ---- Phase 5: 10k alternating ReplaceLinks + UnreadCount rounds,
	// with sampled reference checks (historical messages included).
	start = time.Now()
	checks := 0
	for k := 0; k < nRounds; k++ {
		g := guardianIDs[k%nGuardians]
		set := make([]uint32, campersPerG)
		for j := range set {
			set[j] = campers[rng.Intn(nCampers)].Camper
		}
		c.ReplaceLinks(g, set)
		curLinks[g] = set
		got := c.UnreadCount(g)
		if k%1000 == 0 {
			if want := referenceCount(g); got != want {
				t.Fatalf("round %d: guardian %d unread = %d, want %d", k, g, got, want)
			}
			checks++
		}
	}
	t.Logf("phase rebind+count: %v (%d rounds, %d reference checks)", time.Since(start), nRounds, checks)

	// ---- Phase 6: final sampled verification of 50 guardians ----
	start = time.Now()
	for s := 0; s < 50; s++ {
		g := guardianIDs[rng.Intn(nGuardians)]
		if got, want := c.UnreadCount(g), referenceCount(g); got != want {
			t.Fatalf("final check: guardian %d unread = %d, want %d", g, got, want)
		}
	}
	t.Logf("phase final-check: %v (50 guardians)", time.Since(start))

	// ---- Phase 7: drop driver-side indexes, GC, assert heap budget ----
	csr = nil
	runtime.GC()
	debug.FreeOSMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// Keep the full component state and retained init input alive across
	// the measurement so HeapAlloc reflects the real live heap.
	runtime.KeepAlive(c)
	runtime.KeepAlive(campers)
	runtime.KeepAlive(guardianIDs)
	runtime.KeepAlive(links)
	runtime.KeepAlive(curLinks)
	runtime.KeepAlive(readLists)
	runtime.KeepAlive(camperGroupsOf)
	t.Logf("HeapAlloc after GC: %s (limit %s)", humanBytes(ms.HeapAlloc), humanBytes(heapLimit))
	if ms.HeapAlloc > heapLimit {
		t.Fatalf("HeapAlloc = %s exceeds 192 MiB limit", humanBytes(ms.HeapAlloc))
	}
}

func humanBytes(b uint64) string {
	const mi = 1 << 20
	if b >= mi {
		return fmt.Sprintf("%.1f MiB", float64(b)/mi)
	}
	return fmt.Sprintf("%d B", b)
}
