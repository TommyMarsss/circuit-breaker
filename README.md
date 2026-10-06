# circuit-breaker

服务网格场景下的熔断器（Circuit Breaker）状态机，仅使用 Go 标准库实现，
附带单文件静态 HTML 回放工具（原生 HTML/CSS/JavaScript，无任何框架）。

## 状态机转换规则

```
                错误率 > 阈值 且 样本数 ≥ MinSamples
        ┌──────────────┬─────────────────────────────┐
        ▼              │                             │
   ┌─────────┐   冷却期结束                    ┌──────────┐
   │  OPEN   │──────────────┐                 │  CLOSED  │
   └─────────┘              ▼                 └──────────┘
        ▲              ┌───────────┐                ▲
        │ 任一探测失败  │ HALF-OPEN │  连续成功达阈值  │
        └──────────────└───────────┘────────────────┘
        （冷却计时重置）
```

- **CLOSED（关闭）**：请求正常放行，结果计入滑动窗口。当窗口内
  `请求总数 ≥ MinSamples` 且 `错误率 > ErrorThreshold`（严格大于）时进入 OPEN。
  最小样本数门控保证低流量下单次失败不会误触发。
- **OPEN（打开）**：所有请求快速失败（`Allow` 返回 false）。
  冷却期 `Cooldown` 结束后进入 HALF-OPEN。
  **边界规则**：`now >= openedAt + Cooldown` 即视为冷却结束——恰好落在
  边界上的请求按半开态处理。判定只依赖一次时间比较（`StateAt` 是纯函数），
  同一时刻无论查询多少次结果都一致。
- **HALF-OPEN（半开）**：最多放行 `HalfOpenMaxProbes` 个在途探测请求，
  超额请求被拒绝，防止积压请求同时涌入下游。任一探测失败立即回到 OPEN
  并重置冷却计时；连续成功达到 `HalfOpenSuccessThreshold` 后进入 CLOSED，
  同时重置窗口统计，避免历史失败立刻再次触发。
- **过期许可**：每次进入半开态时代际（generation）递增。上一半开周期
  迟到的探测结果（例如探测失败后其他慢请求才返回成功）会被识别并丢弃，
  不会污染新周期的统计。

## 滑动窗口统计算法

窗口时长 `WindowSize` 被均分为 `WindowBuckets` 个分桶，每个事件按时间
落入槽位 `slot = t / bucketWidth` 对应的桶：

- **增量更新**：窗口维护运行中的 `total`/`fail` 计数。新事件到来时累加；
  时间推进时，槽位 `< curSlot - n + 1` 的桶被判定滑出窗口，其计数从
  运行总计中**增量扣减**，不做全量重算。
- **一致性**：窗口的包含规则以分桶为单位定义（覆盖槽位 `[cur-n+1, cur]`），
  全量重算（`Window.FullCounts`，仅供校验）使用完全相同的槽位规则，
  因此两者结果必然一致——由 `TestWindowIncrementalMatchesFullRecompute`
  用随机事件流逐事件验证，回放页面也把两条曲线叠绘（应完全重合）。
- **重置**：熔断器关闭时调用 `Reset` 清空窗口，重新积累样本。

## 半开态限流策略

`Allow` 在持锁状态下检查并递增 `inFlight`，因此并发竞争下配额不会超发
（`TestHalfOpenProbeQuotaConcurrency` 用 100 个 goroutine 竞争验证放行数
恰好等于上限）。探测结束必须调用 `Report` 归还配额；成功计数与失败
回退同样在锁内完成，保证"任一失败立即回退打开态"的语义在并发下成立。

## 多实例

每个 `Breaker` 实例持有独立的状态、窗口与计数器，不共享任何可变状态，
可安全地按下游服务/端点各建一个实例（`TestMultiInstanceIndependence`）。

## 使用

```go
cfg := breaker.Config{
    WindowSize:                 10 * time.Second,
    WindowBuckets:              10,
    ErrorThreshold:             0.5,
    MinSamples:                 10,
    Cooldown:                   5 * time.Second,
    HalfOpenMaxProbes:          3,
    HalfOpenSuccessThreshold:   3,
}
b := breaker.NewBreaker(cfg, nil) // nil 使用真实时钟

permit, ok := b.Allow()
if !ok {
    // 快速失败：返回降级响应
}
err := callDownstream()
b.Report(permit, err == nil)
```

## 生成回放页面

```sh
go run ./cmd/replay          # 生成 replay.html
open replay.html             # 浏览器打开，逐帧回放
```

回放页面为单一静态 HTML（数据与逻辑全部内嵌，无第三方库），包含：
状态色带、滑动窗口错误率曲线（增量统计与全量重算两条线叠绘校验）、
阈值线、状态切换时刻高亮竖线、成功/失败/拒绝事件点，支持播放、
逐帧步进、变速与多实例切换。

## 测试

```sh
go test ./...
```

| 测试 | 覆盖点 |
|---|---|
| `TestWindowIncrementalMatchesFullRecompute` | 随机事件流下增量统计与全量重算逐事件一致 |
| `TestWindowExpiry` / `TestWindowReset` | 窗口滑出淘汰与重置 |
| `TestMinSamplesGating` | 低流量下 100% 失败也不误触发 |
| `TestOpenOnErrorRate` | 错误率严格大于阈值才打开；打开态快速拒绝 |
| `TestHalfOpenProbeQuotaConcurrency` | 并发竞争下探测配额不超发 |
| `TestHalfOpenFailureResetsCooldown` | 探测失败立即回退打开态且冷却重置 |
| `TestHalfOpenSuccessCloses` | 连续成功达阈值关闭并重置窗口 |
| `TestBoundaryConsistency` | 冷却边界前后 1ns 与重复判定的状态一致性 |
| `TestStaleProbePermitIgnored` | 跨半开周期的迟到结果被丢弃 |
| `TestMultiInstanceIndependence` | 多实例状态机互不影响 |
