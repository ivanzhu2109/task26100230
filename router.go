// Package campbroadcast 实现营地家庭广播的进程内未读计数组件。
//
// 组件只维护路由与已读状态，不保存消息正文；不考虑并发、崩溃与消息删除。
//
// 设计要点：
//   - 组广播只按 (消息 -> <=4 个组) 记录路由，并按 1/2/3/4 元组维护
//     包含-排除计数；发布与换绑均为 O(1)（不遍历监护人或历史消息）。
//   - UnreadCount 用包含-排除在 O(C(16,<=4)) 次表查询内算出当前可见消息数，
//     再减去当前可见的已读数（只遍历该监护人自己的已读集合）。
package campbroadcast

import "sort"

// RouteKind 区分三种合法路由。
type RouteKind int

const (
	// RouteAll 全体监护人可见。
	RouteAll RouteKind = iota
	// RouteOne 仅单一监护人可见。
	RouteOne
	// RouteGroups 1 至 4 个目标组的组广播。
	RouteGroups
)

// Route 描述一条广播的投递路由。
type Route struct {
	Kind RouteKind
	// Guardian 仅当 Kind == RouteOne 时有效，为目标监护人的外部 ID。
	Guardian uint32
	// Groups 仅当 Kind == RouteGroups 时有效，含 1 至 4 个组的外部 ID。
	Groups []uint32
}

const (
	maxGroups       = 4096
	maxCamperGroups = 4
	maxGuardianGrps = 16
	groupWords      = maxGroups / 64
)

// msgRoute 编码（uint64）：
//   - bits 0..1  : 路由类型（0=全体, 1=单播, 2=组广播）
//   - 单播: bits 2..33 为监护人紧凑下标
//   - 组广播: bits 2..49 为 4 个 12 位紧凑组下标（升序，空槽为 0），bits 50..52 为组数
const (
	msgKindAll    = 0
	msgKindOne    = 1
	msgKindGroups = 2
)

// Router 是未读计数组件的完整状态。
type Router struct {
	groupIdx map[uint32]uint16 // 组外部 ID -> 紧凑下标

	camperIdx    map[uint32]uint32 // 营员外部 ID -> camperGroups 下标
	camperGroups [][maxCamperGroups]uint16
	camperNGroup []uint8

	guardianIdx map[uint32]uint32 // 监护人外部 ID -> 下标
	groups      [][]uint16        // 每个监护人当前可访问的紧凑组下标（升序去重，<=16）
	groupBits   [][groupWords]uint64
	directCnt   []uint32
	read        []map[uint32]struct{}

	msgRoute []uint64
	allCnt   uint32

	cnt1     [maxGroups]int32
	tupleCnt u64i32Table
}

// New 以固定的营员—组关系、监护人集合及初始监护人—营员关系初始化组件。
//
// camperGroups 给出每个营员所属的 1..4 个组；guardians 为监护人外部 ID 集合；
// links 为初始的监护人—营员关联（允许空集合，允许缺省表示无关联）。
func New(camperGroups map[uint32][]uint32, guardians []uint32, links map[uint32][]uint32) *Router {
	r := &Router{
		groupIdx:    make(map[uint32]uint16, maxGroups),
		camperIdx:   make(map[uint32]uint32, len(camperGroups)),
		guardianIdx: make(map[uint32]uint32, len(guardians)),
	}
	// 营员与营员—组映射（此后保持不变，供 ReplaceLinks 长期使用）
	camperIDs := make([]uint32, 0, len(camperGroups))
	for id := range camperGroups {
		camperIDs = append(camperIDs, id)
	}
	sort.Slice(camperIDs, func(i, j int) bool { return camperIDs[i] < camperIDs[j] })
	r.camperGroups = make([][maxCamperGroups]uint16, len(camperIDs))
	r.camperNGroup = make([]uint8, len(camperIDs))
	for i, id := range camperIDs {
		gs := camperGroups[id]
		if len(gs) < 1 || len(gs) > maxCamperGroups {
			panic("camper group count out of range")
		}
		r.camperIdx[id] = uint32(i)
		seen := make(map[uint16]bool, len(gs))
		for _, g := range gs {
			cg := r.compactGroup(g)
			if seen[cg] {
				continue
			}
			seen[cg] = true
			r.camperGroups[i][r.camperNGroup[i]] = cg
			r.camperNGroup[i]++
		}
	}
	// 监护人
	n := len(guardians)
	r.groups = make([][]uint16, n)
	r.groupBits = make([][groupWords]uint64, n)
	r.directCnt = make([]uint32, n)
	r.read = make([]map[uint32]struct{}, n)
	for i, id := range guardians {
		if _, dup := r.guardianIdx[id]; dup {
			panic("duplicate guardian id")
		}
		r.guardianIdx[id] = uint32(i)
	}
	// 初始关联
	for g, campers := range links {
		r.ReplaceLinks(g, campers)
	}
	return r
}

func (r *Router) compactGroup(g uint32) uint16 {
	if idx, ok := r.groupIdx[g]; ok {
		return idx
	}
	n := len(r.groupIdx)
	if n >= maxGroups {
		panic("too many groups")
	}
	idx := uint16(n)
	r.groupIdx[g] = idx
	return idx
}

// Publish 发布一条广播，返回递增的消息 ID（从 1 开始）。
func (r *Router) Publish(route Route) uint32 {
	id := uint32(len(r.msgRoute) + 1)
	switch route.Kind {
	case RouteAll:
		r.allCnt++
		r.msgRoute = append(r.msgRoute, msgKindAll)
	case RouteOne:
		gi, ok := r.guardianIdx[route.Guardian]
		if !ok {
			panic("unknown guardian")
		}
		r.directCnt[gi]++
		r.msgRoute = append(r.msgRoute, msgKindOne|uint64(gi)<<2)
	case RouteGroups:
		if len(route.Groups) < 1 || len(route.Groups) > maxCamperGroups {
			panic("group route must have 1..4 groups")
		}
		gs := make([]uint16, 0, len(route.Groups))
		seen := make(map[uint16]bool, len(route.Groups))
		for _, g := range route.Groups {
			cg := r.compactGroup(g)
			if seen[cg] {
				continue
			}
			seen[cg] = true
			gs = append(gs, cg)
		}
		sort.Slice(gs, func(i, j int) bool { return gs[i] < gs[j] })
		r.publishGroups(gs)
	default:
		panic("unknown route kind")
	}
	return id
}

// publishGroups 记录一条组广播：更新 1/2/3/4 元组计数与消息路由编码。
func (r *Router) publishGroups(gs []uint16) {
	var enc uint64 = msgKindGroups | uint64(len(gs))<<50
	for i, g := range gs {
		enc |= uint64(g) << (2 + 12*i)
		r.cnt1[g]++
	}
	r.msgRoute = append(r.msgRoute, enc)
	// 2/3/4 元组子集计数（包含-排除用）
	for i := 0; i < len(gs); i++ {
		for j := i + 1; j < len(gs); j++ {
			r.tupleCnt.add(tupleKey2(gs[i], gs[j]), 1)
			for k := j + 1; k < len(gs); k++ {
				r.tupleCnt.add(tupleKey3(gs[i], gs[j], gs[k]), 1)
				for l := k + 1; l < len(gs); l++ {
					r.tupleCnt.add(tupleKey4(gs[i], gs[j], gs[k], gs[l]), 1)
				}
			}
		}
	}
}

func tupleKey2(a, b uint16) uint64 {
	return 2<<60 | uint64(a)<<12 | uint64(b)
}
func tupleKey3(a, b, c uint16) uint64 {
	return 3<<60 | uint64(a)<<24 | uint64(b)<<12 | uint64(c)
}
func tupleKey4(a, b, c, d uint16) uint64 {
	return 4<<60 | uint64(a)<<36 | uint64(b)<<24 | uint64(c)<<12 | uint64(d)
}

// ReplaceLinks 用给定营员集合替换 guardian 的全部关联，允许空集合。
// 只访问这些营员的固定组映射（<=8 营员 x <=4 组），不触碰消息与已读历史。
func (r *Router) ReplaceLinks(guardian uint32, campers []uint32) {
	gi, ok := r.guardianIdx[guardian]
	if !ok {
		panic("unknown guardian")
	}
	buf := make([]uint16, 0, 8*maxCamperGroups)
	for _, c := range campers {
		ci, ok := r.camperIdx[c]
		if !ok {
			panic("unknown camper")
		}
		ng := r.camperNGroup[ci]
		buf = append(buf, r.camperGroups[ci][:ng]...)
	}
	sort.Slice(buf, func(i, j int) bool { return buf[i] < buf[j] })
	gs := buf[:0]
	for _, g := range buf {
		if len(gs) == 0 || gs[len(gs)-1] != g {
			gs = append(gs, g)
		}
	}
	if len(gs) > maxGuardianGrps {
		panic("guardian accessible groups exceed 16")
	}
	out := make([]uint16, len(gs))
	copy(out, gs)
	r.groups[gi] = out
	var bits [groupWords]uint64
	for _, g := range out {
		bits[g>>6] |= 1 << (g & 63)
	}
	r.groupBits[gi] = bits
}

// MarkRead 在消息当前对 guardian 可见时记录已读并返回 true；
// 重复调用无额外影响；不可见时返回 false 且不改变状态。
func (r *Router) MarkRead(guardian uint32, message uint32) bool {
	gi, ok := r.guardianIdx[guardian]
	if !ok {
		panic("unknown guardian")
	}
	if message < 1 || int(message) > len(r.msgRoute) {
		return false
	}
	if !r.visible(gi, r.msgRoute[message-1]) {
		return false
	}
	rs := r.read[gi]
	if rs == nil {
		rs = make(map[uint32]struct{})
		r.read[gi] = rs
	}
	rs[message] = struct{}{}
	return true
}

// visible 判定编码为 enc 的消息当前对监护人 gi 是否可见。
func (r *Router) visible(gi uint32, enc uint64) bool {
	switch enc & 3 {
	case msgKindAll:
		return true
	case msgKindOne:
		return uint32(enc>>2) == gi
	default: // 组广播：目标组与当前可访问组有交集即可见
		bits := &r.groupBits[gi]
		n := int(enc>>50) & 7
		for i := 0; i < n; i++ {
			g := uint16(enc >> (2 + 12*i) & 0xFFF)
			if bits[g>>6]>>(g&63)&1 == 1 {
				return true
			}
		}
		return false
	}
}

// UnreadCount 返回 guardian 当前可见且未成功标记已读的不同消息数。
func (r *Router) UnreadCount(guardian uint32) uint64 {
	gi, ok := r.guardianIdx[guardian]
	if !ok {
		panic("unknown guardian")
	}
	visible := uint64(r.allCnt) + uint64(r.directCnt[gi]) + r.visibleGroupMsgs(gi)
	var readVisible uint64
	for m := range r.read[gi] {
		if r.visible(gi, r.msgRoute[m-1]) {
			readVisible++
		}
	}
	return visible - readVisible
}

// visibleGroupMsgs 用包含-排除计算当前对该监护人可见的组广播消息数：
// 可见 = |并集 M_g| = c1 - c2 + c3 - c4（每条消息至多 4 个组，展开到 4 级即止）。
func (r *Router) visibleGroupMsgs(gi uint32) uint64 {
	gs := r.groups[gi]
	var c1, c2, c3, c4 int64
	for i := 0; i < len(gs); i++ {
		c1 += int64(r.cnt1[gs[i]])
		for j := i + 1; j < len(gs); j++ {
			c2 += int64(r.tupleCnt.get(tupleKey2(gs[i], gs[j])))
			for k := j + 1; k < len(gs); k++ {
				c3 += int64(r.tupleCnt.get(tupleKey3(gs[i], gs[j], gs[k])))
				for l := k + 1; l < len(gs); l++ {
					c4 += int64(r.tupleCnt.get(tupleKey4(gs[i], gs[j], gs[k], gs[l])))
				}
			}
		}
	}
	return uint64(c1 - c2 + c3 - c4)
}
