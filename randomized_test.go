package broadcast

import (
	"math/rand"
	"testing"
)

// reference is an independent, deliberately simple per-message
// implementation used to cross-check the component.
type reference struct {
	camperGroups map[uint32][]uint32
	links        map[uint32]map[uint32]bool // guardian -> camper set
	routes       []Route                    // index = messageID-1
	read         map[[2]uint32]bool
}

func newReference(campers []CamperInfo, links []GuardianLink) *reference {
	r := &reference{
		camperGroups: make(map[uint32][]uint32),
		links:        make(map[uint32]map[uint32]bool),
		read:         make(map[[2]uint32]bool),
	}
	for _, ci := range campers {
		r.camperGroups[ci.Camper] = ci.Groups
	}
	for _, l := range links {
		r.replaceLinks(l.Guardian, l.Campers)
	}
	return r
}

func (r *reference) replaceLinks(guardian uint32, campers []uint32) {
	s := make(map[uint32]bool, len(campers))
	for _, c := range campers {
		s[c] = true
	}
	r.links[guardian] = s
}

func (r *reference) visible(guardian uint32, route Route) bool {
	switch route.Kind {
	case RouteAllGuardians:
		return true
	case RouteSingleGuardian:
		return route.Guardian == guardian
	default:
		for camper := range r.links[guardian] {
			for _, g := range r.camperGroups[camper] {
				for _, tg := range route.Groups {
					if g == tg {
						return true
					}
				}
			}
		}
		return false
	}
}

func (r *reference) markRead(guardian uint32, message uint32) bool {
	if message == 0 || int(message) > len(r.routes) {
		return false
	}
	if !r.visible(guardian, r.routes[message-1]) {
		return false
	}
	r.read[[2]uint32{guardian, message}] = true
	return true
}

func (r *reference) unreadCount(guardian uint32) uint64 {
	var n uint64
	for i, route := range r.routes {
		id := uint32(i) + 1
		if r.read[[2]uint32{guardian, id}] {
			continue
		}
		if r.visible(guardian, route) {
			n++
		}
	}
	return n
}

// Fixed-seed interleaving of all four operations, cross-checked against
// the reference after every step. Rebinds deliberately switch guardians
// to campers linked to older broadcasts, so historical messages become
// visible after the fact.
func TestRandomizedAgainstReference(t *testing.T) {
	rng := rand.New(rand.NewSource(20261002))

	const (
		nGroups    = 12
		nCampers   = 30
		nGuardians = 8
		nOps       = 4000
	)
	groupIDs := make([]uint32, nGroups)
	for i := range groupIDs {
		groupIDs[i] = 5000 + uint32(i)*17 // arbitrary 32-bit-ish IDs
	}
	camperIDs := make([]uint32, nCampers)
	campers := make([]CamperInfo, nCampers)
	for i := range campers {
		camperIDs[i] = uint32(100 + i)
		ng := 1 + rng.Intn(4)
		gs := make([]uint32, 0, ng)
		for len(gs) < ng {
			g := groupIDs[rng.Intn(nGroups)]
			dup := false
			for _, x := range gs {
				if x == g {
					dup = true
				}
			}
			if !dup {
				gs = append(gs, g)
			}
		}
		campers[i] = CamperInfo{Camper: camperIDs[i], Groups: gs}
	}
	guardianIDs := make([]uint32, nGuardians)
	for i := range guardianIDs {
		guardianIDs[i] = uint32(1 + i)
	}
	randomCamperSet := func() []uint32 {
		n := rng.Intn(9) // 0..8 campers
		out := make([]uint32, 0, n)
		for len(out) < n {
			out = append(out, camperIDs[rng.Intn(nCampers)])
		}
		return out
	}
	links := make([]GuardianLink, nGuardians)
	for i, g := range guardianIDs {
		links[i] = GuardianLink{Guardian: g, Campers: randomCamperSet()}
	}

	c := New(campers, guardianIDs, links)
	ref := newReference(campers, links)

	check := func(step int) {
		for _, g := range guardianIDs {
			got, want := c.UnreadCount(g), ref.unreadCount(g)
			if got != want {
				t.Fatalf("step %d: guardian %d unread = %d, want %d", step, g, got, want)
			}
		}
	}

	var nextMsg uint32 = 1
	for step := 0; step < nOps; step++ {
		switch rng.Intn(4) {
		case 0: // Publish
			var route Route
			switch rng.Intn(5) {
			case 0:
				route = Route{Kind: RouteAllGuardians}
			case 1:
				route = Route{Kind: RouteSingleGuardian, Guardian: guardianIDs[rng.Intn(nGuardians)]}
			default:
				ng := 1 + rng.Intn(4)
				gs := make([]uint32, 0, ng)
				for len(gs) < ng {
					g := groupIDs[rng.Intn(nGroups)]
					dup := false
					for _, x := range gs {
						if x == g {
							dup = true
						}
					}
					if !dup {
						gs = append(gs, g)
					}
				}
				route = Route{Kind: RouteGroups, Groups: gs}
			}
			id := c.Publish(route)
			if id != nextMsg {
				t.Fatalf("step %d: publish id = %d, want %d", step, id, nextMsg)
			}
			nextMsg++
			ref.routes = append(ref.routes, route)
		case 1: // MarkRead (sometimes invalid on purpose)
			g := guardianIDs[rng.Intn(nGuardians)]
			m := uint32(1 + rng.Intn(int(nextMsg)+2)) // may be 0-valid range overrun
			if got, want := c.MarkRead(g, m), ref.markRead(g, m); got != want {
				t.Fatalf("step %d: MarkRead(%d,%d) = %v, want %v", step, g, m, got, want)
			}
		case 2: // ReplaceLinks
			g := guardianIDs[rng.Intn(nGuardians)]
			set := randomCamperSet()
			c.ReplaceLinks(g, set)
			ref.replaceLinks(g, set)
		case 3: // UnreadCount, checked for all guardians below
		}
		check(step)
	}
}
