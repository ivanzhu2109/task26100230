package campbroadcast

import "testing"

// 两名营员共享部分组，一条广播命中多个组；标记已读后换绑到完全不同的组，
// 再恢复原关联，核对去重、不可见标记失败及已读记忆。
func TestRelinkScenario(t *testing.T) {
	const (
		g1, g2         = uint32(1001), uint32(1002)
		c1, c2, c3     = uint32(1), uint32(2), uint32(3)
		ga, gb, gc, gd = uint32(10), uint32(20), uint32(30), uint32(40)
	)
	r := New(
		map[uint32][]uint32{
			c1: {ga, gb}, // 与 c2 共享组 gb
			c2: {gb, gc},
			c3: {gd}, // 完全不同的组
		},
		[]uint32{g1, g2},
		map[uint32][]uint32{g1: {c1, c2}}, // g1 初始可见 {ga,gb,gc}
	)

	m1 := r.Publish(Route{Kind: RouteGroups, Groups: []uint32{ga, gb}}) // 命中 g1 的两个组
	m2 := r.Publish(Route{Kind: RouteGroups, Groups: []uint32{gb, gc}})
	m3 := r.Publish(Route{Kind: RouteGroups, Groups: []uint32{gd}}) // 对 g1 不可见
	mAll := r.Publish(Route{Kind: RouteAll})
	mOne1 := r.Publish(Route{Kind: RouteOne, Guardian: g1})
	mOne2 := r.Publish(Route{Kind: RouteOne, Guardian: g2})

	// 多组命中只计一次：m1,m2,mAll,mOne1 可见，m3,mOne2 不可见
	if got := r.UnreadCount(g1); got != 4 {
		t.Fatalf("initial unread g1 = %d, want 4", got)
	}
	if got := r.UnreadCount(g2); got != 2 { // mAll + mOne2
		t.Fatalf("initial unread g2 = %d, want 2", got)
	}

	// 不可见消息标记失败且不改变状态
	if r.MarkRead(g1, m3) {
		t.Fatal("marking invisible m3 should fail")
	}
	if r.MarkRead(g1, mOne2) {
		t.Fatal("marking others' direct message should fail")
	}
	if got := r.UnreadCount(g1); got != 4 {
		t.Fatalf("unread after failed marks = %d, want 4", got)
	}

	// 标记已读；重复调用无额外影响
	if !r.MarkRead(g1, m1) || !r.MarkRead(g1, m1) {
		t.Fatal("marking visible m1 should succeed")
	}
	if got := r.UnreadCount(g1); got != 3 {
		t.Fatalf("unread after reading m1 = %d, want 3", got)
	}

	// 换绑到完全不同的组：历史可见性立即改变
	r.ReplaceLinks(g1, []uint32{c3})
	if got := r.UnreadCount(g1); got != 3 { // m3 + mAll + mOne1
		t.Fatalf("unread after relink to c3 = %d, want 3", got)
	}
	// 失去可见性的消息不能再标记
	if r.MarkRead(g1, m2) {
		t.Fatal("marking now-invisible m2 should fail")
	}
	// 新获得的 m3 可以标记
	if !r.MarkRead(g1, m3) {
		t.Fatal("marking newly visible m3 should succeed")
	}
	if got := r.UnreadCount(g1); got != 2 {
		t.Fatalf("unread after reading m3 = %d, want 2", got)
	}

	// 恢复原关联：已读记忆保留，m1 不再计入未读，m2 重新计入
	r.ReplaceLinks(g1, []uint32{c1, c2})
	if got := r.UnreadCount(g1); got != 3 { // m2 + mAll + mOne1
		t.Fatalf("unread after restore = %d, want 3", got)
	}

	// 换绑为空集合：只剩全体广播与单播
	r.ReplaceLinks(g1, nil)
	if got := r.UnreadCount(g1); got != 2 { // mAll + mOne1
		t.Fatalf("unread after clearing links = %d, want 2", got)
	}
	if r.MarkRead(g1, mAll) && r.MarkRead(g1, mOne1) {
		if got := r.UnreadCount(g1); got != 0 {
			t.Fatalf("unread after reading all visible = %d, want 0", got)
		}
	} else {
		t.Fatal("marking all/direct messages should succeed")
	}
	_ = m2
	_ = mOne2
}
