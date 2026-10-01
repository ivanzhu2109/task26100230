package broadcast

import "testing"

// Two campers share some groups; one broadcast hits several groups at
// once. Covers dedup across campers/groups, failed marks on invisible
// messages, and read memory surviving rebinds.
func TestScenarioDedupRebindReadMemory(t *testing.T) {
	const (
		gA, gB, gC, gD, gE uint32 = 101, 102, 103, 104, 105
		camper1, camper2   uint32 = 1001, 1002
		camper3            uint32 = 1003
		guardian           uint32 = 7
		other              uint32 = 9
	)
	campers := []CamperInfo{
		{Camper: camper1, Groups: []uint32{gA, gB, gC}},
		{Camper: camper2, Groups: []uint32{gC, gD}}, // shares gC with camper1
		{Camper: camper3, Groups: []uint32{gE}},     // disjoint groups
	}
	guardians := []uint32{guardian, other}
	links := []GuardianLink{{Guardian: guardian, Campers: []uint32{camper1, camper2}}}

	c := New(campers, guardians, links)

	// Broadcast hitting 3 groups, all visible via both campers.
	m1 := c.Publish(Route{Kind: RouteGroups, Groups: []uint32{gA, gC, gD}})
	// Broadcast visible only via camper1.
	m2 := c.Publish(Route{Kind: RouteGroups, Groups: []uint32{gB}})
	// Broadcast invisible to guardian (no linked camper in gE).
	m3 := c.Publish(Route{Kind: RouteGroups, Groups: []uint32{gE}})
	// All-guardian and single-guardian routes.
	m4 := c.Publish(Route{Kind: RouteAllGuardians})
	m5 := c.Publish(Route{Kind: RouteSingleGuardian, Guardian: guardian})
	m6 := c.Publish(Route{Kind: RouteSingleGuardian, Guardian: other})

	if m1 != 1 || m2 != 2 || m3 != 3 || m4 != 4 || m5 != 5 || m6 != 6 {
		t.Fatalf("message IDs not increasing: %d %d %d %d %d %d", m1, m2, m3, m4, m5, m6)
	}

	// m1 counts once although visible via camper1 (gA,gC) and camper2 (gC,gD).
	if got := c.UnreadCount(guardian); got != 4 {
		t.Fatalf("initial unread = %d, want 4 (m1,m2,m4,m5)", got)
	}
	if got := c.UnreadCount(other); got != 2 {
		t.Fatalf("other unread = %d, want 2 (m4,m6)", got)
	}

	// Marking an invisible message fails and changes nothing.
	if ok := c.MarkRead(guardian, m3); ok {
		t.Fatal("MarkRead on invisible message succeeded")
	}
	if got := c.UnreadCount(guardian); got != 4 {
		t.Fatalf("unread after failed mark = %d, want 4", got)
	}

	// Read m1 and m2; repeated marks are no-ops.
	if !c.MarkRead(guardian, m1) || !c.MarkRead(guardian, m2) {
		t.Fatal("MarkRead on visible messages failed")
	}
	if !c.MarkRead(guardian, m1) {
		t.Fatal("repeated MarkRead should be idempotent success")
	}
	if got := c.UnreadCount(guardian); got != 2 {
		t.Fatalf("unread after reads = %d, want 2 (m4,m5)", got)
	}

	// Rebind to a completely disjoint camper: m1,m2 lose visibility, m3
	// becomes visible and unread. Read memory must not resurface as unread.
	c.ReplaceLinks(guardian, []uint32{camper3})
	if got := c.UnreadCount(guardian); got != 3 {
		t.Fatalf("unread after rebind = %d, want 3 (m3,m4,m5)", got)
	}
	// m1 is now invisible: marking must fail.
	if ok := c.MarkRead(guardian, m1); ok {
		t.Fatal("MarkRead on message that lost visibility succeeded")
	}
	if ok := c.MarkRead(guardian, m3); !ok {
		t.Fatal("MarkRead on newly visible m3 failed")
	}
	if got := c.UnreadCount(guardian); got != 2 {
		t.Fatalf("unread after reading m3 = %d, want 2 (m4,m5)", got)
	}

	// Restore original links: m1,m2 stay read (memory kept); m3 loses
	// visibility again. If read memory were lost, count would be 4.
	c.ReplaceLinks(guardian, []uint32{camper1, camper2})
	if got := c.UnreadCount(guardian); got != 2 {
		t.Fatalf("unread after restore = %d, want 2 (m4,m5)", got)
	}

	// Rebind to camper3 once more: m3 is visible again but stays read.
	c.ReplaceLinks(guardian, []uint32{camper3})
	if got := c.UnreadCount(guardian); got != 2 {
		t.Fatalf("unread after second rebind = %d, want 2 (m4,m5)", got)
	}

	// Empty link set: only non-group routes remain visible.
	c.ReplaceLinks(guardian, nil)
	if got := c.UnreadCount(guardian); got != 2 {
		t.Fatalf("unread with empty links = %d, want 2 (m4,m5)", got)
	}
}
