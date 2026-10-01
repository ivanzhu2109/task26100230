# campbroadcast

营地家庭广播的进程内未读计数组件（Go）。监护人通过关联营员获得群组广播的
可见性；换绑营员后历史广播的可见性立即改变。组件只保存路由与已读状态，
不保存消息正文，不考虑并发、崩溃恢复与消息删除。

## API

```go
c := broadcast.New(campers, guardians, links) // 初始化固定关系与初始关联
id := c.Publish(broadcast.Route{...})         // 发布，返回从 1 递增的消息 ID
ok := c.MarkRead(guardianID, messageID)       // 当前可见才成功；重复调用幂等
c.ReplaceLinks(guardianID, camperIDs)         // 整体替换关联（可为空）
n := c.UnreadCount(guardianID)                // 当前可见且未读的不同消息数
```

- `Route.Kind`：`RouteAllGuardians`（全体监护人）、`RouteSingleGuardian`
  （`Route.Guardian` 指定）、`RouteGroups`（`Route.Groups` 1–4 个目标组）。
- 组广播与监护人当前可访问的组（关联营员的组的并集，≤16）有交集即可见。
- 同一消息经多个营员/多个组同时可见只计一次；换绑不丢失已读记忆。

## 设计要点

- 组 ID 内化为 12 位稠密索引（≤4096 组），任意 ≤4 组的子集可打包为
  `uint64` 键。`Publish` 对消息目标组的每个非空子集 T 递增
  `subsetCnt[T]`（≤15 次 map 操作），不遍历监护人、不做逐监护人投递。
- `UnreadCount` 对监护人可见组集合（≤16）做容斥（|T|≤4，共 2516 次
  map 查找）得到可见组广播去重数，加上全体/单人路由计数，再减去该监护人
  已读且当前仍可见的消息数（只扫该监护人自己的已读列表），不重放历史。
- `ReplaceLinks` 仅重算 ≤16 个可见组，O(1)，不触碰消息与已读记录。
- 初始化输入切片由组件持有（计入 HeapAlloc），营员—组映射永久保留以
  支持后续 `ReplaceLinks`。

## 运行

```sh
go test ./...                                  # 全部测试（含规模验证，约 6s）
go test -short ./...                           # 跳过规模验证的快速测试
go test -run TestScenarioDedupRebindReadMemory -v .   # 去重/换绑/已读记忆场景
go test -run TestRandomizedAgainstReference -v .      # 固定种子随机对照
go test -run TestScaleVerification -v -timeout 30m .  # 规模验证
```

规模验证同时打满：10 万营员、4096 组、20 万营员—组关系、2 万监护人、
16 万当前关联、100 万消息、396 万消息—目标组关系、100 万成功已读对；
广播混合大量重叠热门组与稀疏组；完成 1 万次换绑+计数交替并抽样核对；
断言 GC 后 HeapAlloc < 192 MiB 并报告各阶段耗时。本机实测约
85 MiB，全程约 4s。
