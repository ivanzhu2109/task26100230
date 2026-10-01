# campbroadcast — 营地家庭广播进程内未读计数组件

Go 实现的进程内组件：只维护广播路由与已读状态（不保存消息正文），
支持监护人—营员关联的即时换绑，换绑后历史广播可见性立即随之改变。
不考虑并发、崩溃恢复与消息删除。

## 数据模型与接口

```go
// 初始化：固定的营员—组关系、监护人集合、初始监护人—营员关联
r := campbroadcast.New(camperGroups, guardians, links)
// camperGroups: map[uint32][]uint32  每个营员所属 1..4 个组（此后不变）
// guardians:    []uint32             监护人 ID 集合
// links:        map[uint32][]uint32  初始关联（监护人 -> 营员集合，可为空）

id := r.Publish(campbroadcast.Route{Kind: campbroadcast.RouteAll})
id  = r.Publish(campbroadcast.Route{Kind: campbroadcast.RouteOne, Guardian: g})
id  = r.Publish(campbroadcast.Route{Kind: campbroadcast.RouteGroups, Groups: []uint32{g1, g2}}) // 1..4 个组

ok := r.MarkRead(guardian, id)   // 当前可见才成功；重复调用无额外影响
r.ReplaceLinks(guardian, campers) // 整体替换关联，允许空集合
n  := r.UnreadCount(guardian)     // 当前可见且未读的不同消息数
```

- 消息 ID 从 1 开始递增；所有实体 ID 为 32 位整数（组件内部做紧凑化）。
- 组广播与监护人**当前**可访问的组（关联营员所属组的并集，≤16）有交集即可见；
  同一消息经多个营员/多个组同时可见只计一次。
- 已读记忆与关联解耦：换绑不清除已读，重新获得访问权不会让已读消息再次计为未读。

## 设计要点

- **发布 O(1)**：组广播只记录 `消息 -> ≤4 个组` 的路由编码，并按 1/2/3/4 元组
  累加包含-排除计数（单组用数组，2/3/4 元组用紧凑开放寻址哈希表，
  见 `table.go`）。不遍历监护人，不展开逐监护人投递记录。
- **换绑 O(关联营员数)**：只重算该监护人 ≤8 名营员的组并集（≤16 组）
  与 4096 位可见组位图，不触碰历史消息与已读记录。
- **计数 O(C(16,≤4) + 自身已读数)**：可见消息数 = 全体广播数 + 单播数 +
  组广播的包含-排除（c1−c2+c3−c4，约 2500 次表查询）；再减去当前可见的
  已读数（只遍历该监护人自己的已读集合，逐条做 O(1) 位图可见性判定）。
  不重放发布/换绑历史，不扫描全部消息。
- **内存**：消息路由 8 B/条；元组表约 12 B/条目；已读为每监护人
  `map[uint32]struct{}`。规模验证实测完整状态（含保留的初始化输入）
  GC 后 HeapAlloc ≈ 120 MiB < 192 MiB。

## 测试与规模验证

```sh
# 全部单元测试（换绑场景、固定种子随机交错操作对拍、哈希表）
go test -v ./...

# 规模验证（各维度同时达上限，1.2 万次换绑+计数交替，抽样核对，内存断言）
go test -run TestScale -v -timeout 600s ./...
```

规模验证覆盖：10 万营员 / 20 万条营员-组关系 / 4096 组；2 万监护人 /
16 万条当前关联；100 万条消息 / 400 万条消息-组关系（85% 热门组高度重叠
+ 15% 热门与稀疏混合）；100 万对成功已读（含换绑后不可见的记录）；
完整初始化输入全程保留并计入 HeapAlloc；分阶段耗时随测试日志输出。
