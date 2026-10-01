package campbroadcast

import (
	"math/rand"
	"testing"
)

// naive 是独立的逐消息参考实现：直接按题意暴力计算。
type naive struct {
	routes   []Route // 下标 = 消息ID-1
	links    map[uint32][]uint32
	camperGr map[uint32][]uint32
	read     map[uint32]map[uint32]bool
}

func (n *naive) visible(g uint32, rt Route) bool {
	switch rt.Kind {
	case RouteAll:
		return true
	case RouteOne:
		return rt.Guardian == g
	}
	gs := make(map[uint32]bool)
	for _, c := range n.links[g] {
		for _, grp := range n.camperGr[c] {
			gs[grp] = true
		}
	}
	for _, grp := range rt.Groups {
		if gs[grp] {
			return true
		}
	}
	return false
}

// 固定随机种子交错执行四种操作，每次操作后都与参考实现核对计数。
func TestRandomizedCrossCheck(t *testing.T) {
	rng := rand.New(rand.NewSource(20261002))

	const nGroups, nCampers, nGuardians = 30, 40, 25
	camperGr := make(map[uint32][]uint32)
	for c := uint32(1); c <= nCampers; c++ {
		k := 1 + rng.Intn(4)
		seen := map[uint32]bool{}
		for len(seen) < k {
			g := uint32(rng.Intn(nGroups))
			if !seen[g] {
				seen[g] = true
				camperGr[c] = append(camperGr[c], g)
			}
		}
	}
	guardians := make([]uint32, nGuardians)
	for i := range guardians {
		guardians[i] = uint32(1000 + i)
	}
	// 生成满足“并集组数 <= 16”约束的随机关联集合
	randLinks := func() []uint32 {
		for {
			k := rng.Intn(9)
			seen := map[uint32]bool{}
			var campers []uint32
			for len(seen) < k {
				c := uint32(1 + rng.Intn(nCampers))
				if !seen[c] {
					seen[c] = true
					campers = append(campers, c)
				}
			}
			grps := map[uint32]bool{}
			for _, c := range campers {
				for _, grp := range camperGr[c] {
					grps[grp] = true
				}
			}
			if len(grps) <= 16 {
				return campers
			}
		}
	}
	links := make(map[uint32][]uint32)
	for _, g := range guardians {
		links[g] = randLinks()
	}

	r := New(camperGr, guardians, links)
	ref := &naive{links: map[uint32][]uint32{}, camperGr: camperGr, read: map[uint32]map[uint32]bool{}}
	for g, cs := range links {
		ref.links[g] = append([]uint32(nil), cs...)
	}

	checkAll := func(step int) {
		for _, g := range guardians {
			want := uint64(0)
			for i, rt := range ref.routes {
				if ref.visible(g, rt) && !ref.read[g][uint32(i+1)] {
					want++
				}
			}
			if got := r.UnreadCount(g); got != want {
				t.Fatalf("step %d guardian %d: got %d want %d", step, g, got, want)
			}
		}
	}

	const steps = 1500
	for s := 0; s < steps; s++ {
		g := guardians[rng.Intn(len(guardians))]
		switch rng.Intn(4) {
		case 0: // Publish
			var rt Route
			switch rng.Intn(3) {
			case 0:
				rt = Route{Kind: RouteAll}
			case 1:
				rt = Route{Kind: RouteOne, Guardian: guardians[rng.Intn(len(guardians))]}
			default:
				k := 1 + rng.Intn(4)
				seen := map[uint32]bool{}
				for len(seen) < k {
					grp := uint32(rng.Intn(nGroups))
					if !seen[grp] {
						seen[grp] = true
						rt.Groups = append(rt.Groups, grp)
					}
				}
				rt.Kind = RouteGroups
			}
			id := r.Publish(rt)
			if int(id) != len(ref.routes)+1 {
				t.Fatalf("step %d: message id %d, want %d", s, id, len(ref.routes)+1)
			}
			ref.routes = append(ref.routes, rt)
		case 1: // MarkRead
			if len(ref.routes) == 0 {
				continue
			}
			m := uint32(1 + rng.Intn(len(ref.routes)))
			got := r.MarkRead(g, m)
			want := ref.visible(g, ref.routes[m-1])
			if got != want {
				t.Fatalf("step %d: MarkRead(%d,%d) = %v, want %v", s, g, m, got, want)
			}
			if want {
				if ref.read[g] == nil {
					ref.read[g] = map[uint32]bool{}
				}
				ref.read[g][m] = true
			}
		case 2: // ReplaceLinks
			campers := randLinks()
			r.ReplaceLinks(g, campers)
			ref.links[g] = campers
		case 3: // UnreadCount（在 checkAll 中统一核对）
		}
		if s%7 == 0 {
			checkAll(s)
		}
	}
	checkAll(steps)
}

// 换绑后必须能看见发布于关联建立之前的消息（且未读过则计为未读）。
func TestHistoricalMessageAfterRelink(t *testing.T) {
	const g, c1, c2 = uint32(7), uint32(1), uint32(2)
	r := New(
		map[uint32][]uint32{c1: {11}, c2: {22}},
		[]uint32{g},
		map[uint32][]uint32{g: {c1}},
	)
	m := r.Publish(Route{Kind: RouteGroups, Groups: []uint32{22}}) // 发布时 g 尚无 c2 关联
	if got := r.UnreadCount(g); got != 0 {
		t.Fatalf("before relink unread = %d, want 0", got)
	}
	r.ReplaceLinks(g, []uint32{c2})
	if got := r.UnreadCount(g); got != 1 {
		t.Fatalf("after relink unread = %d, want 1 (历史消息应可见)", got)
	}
	if !r.MarkRead(g, m) {
		t.Fatal("newly visible historical message should be markable")
	}
	r.ReplaceLinks(g, []uint32{c1})
	r.ReplaceLinks(g, []uint32{c2})
	if got := r.UnreadCount(g); got != 0 {
		t.Fatalf("read memory lost after relink cycle, unread = %d, want 0", got)
	}
}
