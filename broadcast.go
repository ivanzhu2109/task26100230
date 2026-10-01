// Package broadcast implements an in-process unread-counter for camp
// family broadcasts. Guardians see group broadcasts through their linked
// campers; re-linking campers changes visibility of historical broadcasts
// immediately. The component stores only routes and read state, never
// message bodies.
//
// Design notes:
//   - Group IDs are interned to dense 12-bit indices (at most 4096 groups),
//     so any subset of up to 4 groups packs into a uint64 key.
//   - Publish increments, for every non-empty subset T of the message's
//     target groups, subsetCnt[T] = number of published messages whose
//     group set is a superset of T. This is O(1) per publish (<=15 map
//     increments) and never touches guardians.
//   - A guardian's visible group set S has at most 16 groups, so the
//     number of distinct visible group-broadcasts is computed by
//     inclusion-exclusion over subsets of S of size <=4
//     (16+120+560+1820 = 2516 map lookups), without scanning messages.
//   - Reads are stored per guardian (dedup set + per-guardian list).
//     UnreadCount subtracts the guardian's read messages that are still
//     visible; it scans only that guardian's own read list, never the
//     message history. ReplaceLinks only recomputes the <=16-group set.
package broadcast

import "sort"

// RouteKind identifies the shape of a publish route.
type RouteKind uint8

const (
	// RouteAllGuardians reaches every guardian.
	RouteAllGuardians RouteKind = iota
	// RouteSingleGuardian reaches exactly one guardian.
	RouteSingleGuardian
	// RouteGroups reaches guardians whose accessible groups intersect
	// the 1..4 target groups.
	RouteGroups
)

// Route describes where a message is published.
type Route struct {
	Kind     RouteKind
	Guardian uint32   // target for RouteSingleGuardian
	Groups   []uint32 // 1..4 target groups for RouteGroups
}

// CamperInfo binds a camper to its fixed set of groups (at most 4).
type CamperInfo struct {
	Camper uint32
	Groups []uint32
}

// GuardianLink is one initial guardian -> campers binding.
type GuardianLink struct {
	Guardian uint32
	Campers  []uint32
}

// msgRecord is the stored per-message route (16 bytes).
type msgRecord struct {
	groups  [4]uint16 // interned, sorted, deduplicated target groups
	target  uint32    // guardian ID for RouteSingleGuardian
	nGroups uint8
	kind    uint8
}

// guardianState holds everything tracked per guardian.
type guardianState struct {
	campers   []uint32 // current linked campers (<=8)
	groups    []uint16 // sorted interned visible groups (<=16)
	singleCnt uint64   // messages routed to this guardian alone
	reads     []uint32 // successfully read message IDs (history kept)
}

// Component is the in-process unread counter. Not safe for concurrent use.
type Component struct {
	// retained initialization input (counted toward HeapAlloc)
	camperInfos []CamperInfo
	guardianIDs []uint32
	initLinks   []GuardianLink

	camperGroups map[uint32][]uint16 // camper -> interned groups (kept forever)
	groupIndex   map[uint32]uint16   // group ID -> dense index
	groupIDs     []uint32            // dense index -> group ID

	guardians map[uint32]*guardianState
	messages  []msgRecord
	subsetCnt map[uint64]uint32 // packed group subset -> #published supersets
	pubAll    uint64            // messages routed to all guardians
	readSet   map[uint64]struct{}
}

// New builds a Component from fixed camper-group relations, the guardian
// set, and the initial guardian-camper links. The input slices are
// retained for the lifetime of the component.
func New(campers []CamperInfo, guardians []uint32, links []GuardianLink) *Component {
	c := &Component{
		camperInfos:  campers,
		guardianIDs:  guardians,
		initLinks:    links,
		camperGroups: make(map[uint32][]uint16, len(campers)),
		groupIndex:   make(map[uint32]uint16, 4096),
		guardians:    make(map[uint32]*guardianState, len(guardians)),
		subsetCnt:    make(map[uint64]uint32),
		readSet:      make(map[uint64]struct{}),
	}
	for _, ci := range campers {
		gs := make([]uint16, 0, len(ci.Groups))
		for _, gid := range ci.Groups {
			idx := c.intern(gid)
			if !containsU16(gs, idx) {
				gs = append(gs, idx)
			}
		}
		c.camperGroups[ci.Camper] = gs
	}
	for _, gid := range guardians {
		c.guardians[gid] = &guardianState{}
	}
	for _, l := range links {
		c.ReplaceLinks(l.Guardian, l.Campers)
	}
	return c
}

// Publish stores the message route and returns an increasing message ID
// (starting at 1).
func (c *Component) Publish(r Route) uint32 {
	id := uint32(len(c.messages)) + 1
	rec := msgRecord{kind: uint8(r.Kind), target: r.Guardian}
	switch r.Kind {
	case RouteAllGuardians:
		c.pubAll++
	case RouteSingleGuardian:
		if g, ok := c.guardians[r.Guardian]; ok {
			g.singleCnt++
		}
	case RouteGroups:
		var gs [4]uint16
		n := 0
		for _, gid := range r.Groups {
			if n == 4 {
				break
			}
			idx := c.intern(gid)
			if !containsU16(gs[:n], idx) {
				gs[n] = idx
				n++
			}
		}
		sortU16(gs[:n])
		rec.groups = gs
		rec.nGroups = uint8(n)
		// Increment subset counters for every non-empty subset of the
		// target groups (at most 15 increments).
		for mask := 1; mask < 1<<uint(n); mask++ {
			var key uint64
			shift := uint(0)
			for i := 0; i < n; i++ {
				if mask&(1<<uint(i)) != 0 {
					key |= uint64(gs[i]) << shift
					shift += 12
				}
			}
			c.subsetCnt[key]++
		}
	}
	c.messages = append(c.messages, rec)
	return id
}

// MarkRead records the message as read for the guardian when it is
// currently visible. Returns false (changing nothing) when the message is
// not visible. Repeating a successful mark has no extra effect.
func (c *Component) MarkRead(guardian, message uint32) bool {
	g, ok := c.guardians[guardian]
	if !ok {
		return false
	}
	if message == 0 || int(message) > len(c.messages) {
		return false
	}
	if !visibleTo(g, guardian, &c.messages[message-1]) {
		return false
	}
	key := uint64(guardian)<<32 | uint64(message)
	if _, ok := c.readSet[key]; ok {
		return true // already read: idempotent success, no state change
	}
	c.readSet[key] = struct{}{}
	g.reads = append(g.reads, message)
	return true
}

// ReplaceLinks replaces all guardian-camper links for the guardian with
// the given camper set (possibly empty). It only recomputes the guardian's
// visible group set (<=16 entries); it never scans messages or reads.
func (c *Component) ReplaceLinks(guardian uint32, campers []uint32) {
	g, ok := c.guardians[guardian]
	if !ok {
		return
	}
	cp := make([]uint32, len(campers))
	copy(cp, campers)
	g.campers = cp
	set := make(map[uint16]struct{}, 16)
	for _, cid := range campers {
		for _, gi := range c.camperGroups[cid] {
			set[gi] = struct{}{}
		}
	}
	gs := make([]uint16, 0, len(set))
	for gi := range set {
		gs = append(gs, gi)
	}
	sortU16(gs)
	g.groups = gs
}

// UnreadCount returns the number of distinct currently-visible messages
// not successfully marked read by the guardian.
func (c *Component) UnreadCount(guardian uint32) uint64 {
	g, ok := c.guardians[guardian]
	if !ok {
		return 0
	}
	visible := c.pubAll + g.singleCnt + c.visibleGroupBroadcasts(g.groups)
	var readVisible uint64
	for _, mid := range g.reads {
		if visibleTo(g, guardian, &c.messages[mid-1]) {
			readVisible++
		}
	}
	return visible - readVisible
}

// visibleGroupBroadcasts counts distinct published group broadcasts whose
// target groups intersect s (sorted interned groups, |s|<=16) via
// inclusion-exclusion over subsets of s of size <=4.
func (c *Component) visibleGroupBroadcasts(s []uint16) uint64 {
	n := len(s)
	var total int64
	for i := 0; i < n; i++ {
		total += int64(c.subsetCnt[uint64(s[i])])
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			total -= int64(c.subsetCnt[uint64(s[i])|uint64(s[j])<<12])
		}
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			for k := j + 1; k < n; k++ {
				total += int64(c.subsetCnt[uint64(s[i])|uint64(s[j])<<12|uint64(s[k])<<24])
			}
		}
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			for k := j + 1; k < n; k++ {
				for l := k + 1; l < n; l++ {
					total -= int64(c.subsetCnt[uint64(s[i])|uint64(s[j])<<12|uint64(s[k])<<24|uint64(s[l])<<36])
				}
			}
		}
	}
	return uint64(total)
}

// visibleTo reports whether the message is currently visible to the guardian.
func visibleTo(g *guardianState, guardian uint32, rec *msgRecord) bool {
	switch RouteKind(rec.kind) {
	case RouteAllGuardians:
		return true
	case RouteSingleGuardian:
		return rec.target == guardian
	default:
		for _, gi := range rec.groups[:rec.nGroups] {
			if containsU16(g.groups, gi) {
				return true
			}
		}
		return false
	}
}

// intern maps a 32-bit group ID to a dense 12-bit index.
func (c *Component) intern(gid uint32) uint16 {
	if idx, ok := c.groupIndex[gid]; ok {
		return idx
	}
	idx := uint16(len(c.groupIDs))
	c.groupIndex[gid] = idx
	c.groupIDs = append(c.groupIDs, gid)
	return idx
}

func containsU16(s []uint16, v uint16) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func sortU16(s []uint16) {
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
}
