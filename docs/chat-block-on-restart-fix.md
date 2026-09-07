# 后端修复方案：重启沙箱后会话阻塞 ~5 分钟

## 一、根因（复核确认）

沙箱重启后，机器侧 `agentwork connect` 进程死亡，但 daemon 侧的 chat entry（`d.chat.chats[chatID]`）不被清理。用户下次发消息时：

1. `ChatWrite`（`internal/daemon/chat.go:294`）查到**残留** entry → `MachinePeer(machineID)` 返回**新 peer**（机器已重注册）→ 帧转发到新进程 → 新进程的 `chatBridge` 是空 map → CLI 端 `cmd/agentwork-cli/chat.go:182` 返回 `"unknown chat chat-1"`。
2. `internal/server/server.go:410-413` 的 handler 收到错误后 `log + return`，触发 `defer` 链：
   - `close(pingDone)`（`server.go:387`）—— ping goroutine 停止，WebSocket **双向静默**。
   - `CloseChat`（`chat.go:443`）—— 删 entry、通知机器，但**不关 web WS**。
3. 前端 `web/lib/acp.ts:120-129` 的 `send()` 无超时，`prompt` 的 Promise 永久 pending。`onclose`（`acp.ts:94`）是唯一解锁触发器。
4. 双向零流量持续 ~300s → **浏览器链路的代理空闲超时**（`server.go:341` 注释自证 "nginx 300s default"）切断 TCP → 浏览器 `onclose` → UI 解锁。

> 5:01 / 5:01 / 5:16 三个间隔精确吻合 300s + ~1s 重连，不是 Go GC finalizer（GC 时序必然抖动，且 sysmon 强制 GC 上限 2 分钟）。

### 隐藏变体（更严重）

机器在 **turn 进行中**死亡时，handler 仍阻塞在 `ReadMessage`（`server.go:400`），ping goroutine **仍在运行**——每 30s 的 ping 持续刷新代理空闲计时器，连接**永远不会**被判空闲。结果：prompt 无限挂起，只能手动刷新页面。残留 chat 不清理 + `onMachineOffline` 不关 WS 是此变体的直接原因。

---

## 二、修复目标

| 编号 | 目标 | 覆盖场景 |
|------|------|----------|
| A | `ChatWrite` 出错路径立即关 web WS | turn 之间发消息触发的 5 分钟阻塞（秒级恢复） |
| B | 机器 peer 被替换/注销时清扫其残留 chats | 上面 A + turn 中途死亡的无限挂起变体 |
| C | `CloseChat` 补 `e.close()` | 防御性兜底：任何走 web-side 关闭路径的都关 WS |

A 是 B 的子集（A 修的是"发消息才暴露"这一条路径，B 修的是"机器一下线就主动清"）。但 A 改动最小、风险最低、能独立交付；B 是治本。**推荐 A+C 一起合入，B 作为治本增强。**

---

## 三、详细改动

### Fix A：`ChatWrite` 出错时显式关 WS（核心，最小改动）

**文件**：`internal/server/server.go`

**位置**：`/agents/{id}/acp` handler 的 web→machine 循环（`server.go:399-414`）

**改动**：`ChatWrite` 返回错误时，显式 `conn.Close()`，不依赖 `defer CloseChat`（后者不关 WS）。

```go
for {
    _, msg, err := conn.ReadMessage()
    if err != nil {
        logging.Infof("chat: %s web read: %v", chatID, err)
        return
    }
    if err := s.d.ChatWrite(chatID, msg); err != nil {
        logging.Infof("chat: %s web→machine: %v", chatID, err)
        // The machine-side chat died (restart → "unknown chat", or the
        // link dropped mid-turn). The relay can no longer forward, but
        // the web socket is still open and the frontend's prompt Promise
        // is pending on onclose. Close NOW so the browser reconnects in
        // seconds instead of waiting ~300s for the proxy idle timeout.
        // CloseChat (deferred) already tears down the machine-side entry;
        // this closes the WEB side.
        _ = conn.Close()
        return
    }
}
```

**为什么安全**：
- `conn.Close()` 后，`ReadMessage` 立即返回错误，handler 走正常的 `return` → `defer` 链（`close(pingDone)` + `CloseChat`）正常执行。无资源泄漏。
- `gorilla/websocket` 的 `Close` 可与并发的 `WriteControl`（ping goroutine）安全并发（doc.go 明确说明，`server.go:346-348` 注释也引用了这点）。
- `BindChatSink` 的 writer pump 在 `write` 失败时本就会 `closeFn()`（`chat.go:187-191`），这里只是把"等代理超时"变成"立即"。

**效果**：14:21:05 当场 `onclose` → 前端秒级显示"连接已断开" → 用户重开即恢复。

---

### Fix C：`CloseChat` 补 `e.close()`（防御性兜底）

**文件**：`internal/daemon/chat.go`

**位置**：`CloseChat`（`chat.go:443-456`）

**改动**：与 `MachineChatClosed`（`chat.go:436-439`）对齐，补上 `e.close()`。

```go
// CloseChat tears the machine chat down (the web socket disconnected).
func (d *Daemon) CloseChat(chatID string) {
    d.chat.mu.Lock()
    e, ok := d.chat.chats[chatID]
    delete(d.chat.chats, chatID)
    d.chat.mu.Unlock()
    if e == nil {
        return
    }
    e.closeOnce.Do(func() { close(e.done) })
    if peer := d.MachinePeer(e.machineID); peer != nil {
        _ = peer.Notify(context.Background(), link.MethodChatClose, link.ChatCloseParams{ChatID: chatID})
    }
    if e.close != nil {
        e.close() // close the web socket — mirrors MachineChatClosed.
    }
    logging.Infof("chat: %s closed (web-side)", chatID)
}
```

**为什么安全**：
- `e.close` 由 `BindChatSink`（`chat.go:162`）设置为 `func() { _ = conn.Close() }`（`server.go:396`）。`conn.Close()` 幂等且可并发。
- `e.closeOnce` 保证 `done` 只 close 一次；`e.close()` 在 `closeOnce.Do` 之后调用，与 `MachineChatClosed` 顺序一致。
- 原注释 "the web socket disconnected" 的前提（WS 已断）在正常路径成立，但在 `ChatWrite` 错误路径不成立——这正是 bug。补上后，所有走 `CloseChat` 的路径都确保 WS 关闭。
- **Fix A 已让 `ChatWrite` 错误路径的 `conn.Close()` 先于 `defer CloseChat` 执行**，所以 C 在该路径上是冗余的二次关闭（无害）；C 的价值在于覆盖**其它**可能新增的、不经 A 的关闭路径。

**与 Fix A 的关系**：A 修了已知的错误路径，C 是"任何走 CloseChat 的路径都关 WS"的契约保证。两者无冲突，C 让 A 之外的未来路径也安全。

---

### Fix B：机器 peer 替换时清扫残留 chats（治本增强）

**目标**：机器一下线（旧 peer 被替换或注销）就主动关掉它身上的所有 open chat，覆盖 turn 中途死亡的无限挂起变体。

#### B.1 竞态约束（关键）

`machine:offline` 事件可能**滞后**（`machine.go:151-155` 注释自承）：死掉的旧 link 发的 offline notification 可能在新 peer 重注册后才处理。`MarkOffline` 的 `flipped` 标志（只翻转 `connected→offline`）能过滤掉"机器已重连"的滞后误报，但**在翻转与重连之间存在窗口**。

因此 B **不能**挂在 `onMachineOffline` 异步事件上（会误杀新 peer 上的新 chat）。必须挂在 **peer 身份替换**的那一刻——旧 peer 被替换时，它身上的 chat 必然全部失效，无竞态。

#### B.2 实现：给 chatEntry 加 peer 身份字段

**文件**：`internal/daemon/chat.go`

**改动 1**：`chatEntry` 加字段（`chat.go:33-62`）：

```go
type chatEntry struct {
    machineID string
    peer      *link.Peer // identity of the machine link that opened this chat —
                         // set in OpenChatForAgent; a replaced peer means this
                         // chat's machine-side half is dead.
    cwd       string
    // ... 其余不变
}
```

**改动 2**：`OpenChatForAgent` 设置 `peer`（`chat.go:137-144`）：

```go
d.chat.chats[res.ChatID] = &chatEntry{
    machineID: machineID,
    peer:      peer, // captured BEFORE the Call — the peer that owns this chat
    cwd:       res.Cwd,
    // ...
}
```

> 注意：`peer` 在 `OpenChatForAgent:93` 取得，`Call` 在 `:122`。即便 Call 期间 peer 被替换，Call 会失败（旧 peer 已 Close），不会创建 entry。所以 entry 里的 peer 恒等于"成功打开 chat 的那个 peer"。

**改动 3**：新增清扫方法（chat.go，紧跟 `MachineChatClosed`）：

```go
// closeChatsForPeer tears down every chat whose machine link was THIS peer.
// Called when a peer is replaced or unregistered: the machine-side chat
// processes died with the link, so the web sockets must close NOW (the
// frontend's prompt Promise is pending on onclose). Identity-matched by
// peer pointer — a 滞后 offline event from a dead link must NOT kill chats
// opened on the live replacement (onMachineOffline would; this does not).
func (d *Daemon) closeChatsForPeer(p *link.Peer) {
    d.chat.mu.Lock()
    var toClose []*chatEntry
    for id, e := range d.chat.chats {
        if e.peer == p {
            toClose = append(toClose, e)
            delete(d.chat.chats, id)
        }
    }
    d.chat.mu.Unlock()
    for _, e := range toClose {
        e.closeOnce.Do(func() { close(e.done) })
        if e.close != nil {
            e.close()
        }
        logging.Infof("chat: machine link replaced — closed web socket for chat on %s", e.machineID)
    }
}
```

> 不发 `chat.close` 通知机器——旧 peer 已死，通知无去处；新 peer 上没有这些 chat。只关 web 侧（与 `MachineChatClosed` 对称，后者也不通知机器）。

**文件**：`internal/daemon/machine_dispatch.go`

**改动 4**：`RegisterMachinePeer` 替换旧 peer 时调用清扫（`machine_dispatch.go:30-40`）：

```go
func (d *Daemon) RegisterMachinePeer(machineID string, p *link.Peer) {
    d.machineMu.Lock()
    if d.machinePeers == nil {
        d.machinePeers = map[string]*link.Peer{}
    }
    var stale *link.Peer
    if old, ok := d.machinePeers[machineID]; ok && old != p {
        stale = old
    }
    d.machinePeers[machineID] = p
    d.machineMu.Unlock()
    if stale != nil {
        // The machine reconnected with a NEW link — the old link's chat
        // processes are dead. Close their web sockets before the old peer
        // is fully torn down (old.Close() below races the chat.close
        // notifications we no longer need to send). Close first, then
        // drop the link.
        d.closeChatsForPeer(stale)
        stale.Close()
    }
}
```

> 把 `old.Close()` 移到锁外、清扫之后：避免持锁关 WS（WS 关闭回调可能反向触达 daemon）。`UnregisterMachinePeer`（`machine_dispatch.go:46-52`）**不**加清扫——它在 `/connect` 的 `defer`（`server.go:198`）里执行，那时 handler 已退出，peer 本就没了，且 `RegisterMachinePeer` 的替换路径已覆盖"重连"场景；`Unregister` 只兜底"机器彻底没回来"的最终清理，此时 chats 早已被 A 或代理超时关掉。若想双保险，可在 `UnregisterMachinePeer` 也调一次 `closeChatsForPeer(p)`（幂等，已关的 entry 已从 map 删除）。

**为什么安全**：
- 按 **peer 指针身份**匹配，不按 machineID——滞后的 offline 事件走的是 `onMachineOffline`（不改），不会触达此路径；新 peer 上的新 chat 的 `e.peer` 是新指针，不被匹配。
- `closeChatsForPeer` 在 `machineMu` 锁外执行（只持 `chat.mu`），与 `RegisterMachinePeer` 无锁嵌套。
- `e.close` / `e.closeOnce` 的并发安全性与 Fix C 相同。

**效果**：机器重启 → 新 connect 注册 → `RegisterMachinePeer` 替换旧 peer → `closeChatsForPeer` 立即关掉所有残留 chat 的 web WS → 前端 `onclose` 秒级触发。turn 中途死亡变体也被覆盖（peer 替换是同步信号，不依赖心跳/超时）。

---

## 四、不做什么（纠正上一轮建议）

| 上一轮建议 | 复核结论 |
|-----------|----------|
| 前端给 `prompt` 加 60s 超时 | **不要**。`acp.ts:126-129` 注释明确设计"turn 要多久就多久"，60s 会杀掉合法长 turn。正确做法是只给 setup RPC（connect/initialize、session/new、session/load）加 ~30s 超时——但这是前端改动，不在本后端方案范围。 |
| ~5 分钟来自 GC finalizer | **错**。是 ~300s 代理空闲超时（`server.go:341` 自证）。GC 解释不了 5:01/5:01 的精确一致性。 |
| 前端 `acpStoreCommon.ts`/`attemptAgentReconnect`/3 次退避重连 | **不存在**。真实前端是 `web/lib/acp.ts` + `web/components/chat-panel.tsx`，`onClose` 只报"连接已断开"不自动重连。 |

---

## 五、验证计划

1. **单元测试**（新增 `internal/daemon/chat_close_test.go`）：
   - `TestCloseChatClosesWebSocket`：mock `e.close`，断言 `CloseChat` 后被调用。
   - `TestChatWriteErrorClosesConn`：mock `ChatWrite` 返回 error，断言 handler 调了 `conn.Close()`（可用 `net.Pipe` + 手写 WS 帧或 httptest）。
   - `TestCloseChatsForPeer`：两个 chat 分属不同 peer，替换 peer1，断言 peer1 的 chat WS 关闭、peer2 的不受影响。
   - `TestCloseChatsForPeerIgnoresNewPeerChat`：旧 peer 死后新 peer 打开新 chat，再触发旧 peer 清扫，断言新 chat 不被误杀。

2. **端到端**（手动）：
   - 起 daemon + 一个沙箱，打开 chat，发消息确认正常。
   - 重启沙箱（kill connect 进程），立刻发消息 → 断言 < 5s 内前端显示"连接已断开"（Fix A）。
   - 重启沙箱后**不**发消息，等新 connect 重注册 → 断言前端自动 onclose（Fix B，无需用户发消息触发）。
   - turn 进行中 kill connect → 断言前端 onclose 而非无限挂起（Fix B 变体）。

3. **回归**：跑 `internal/daemon` 全部测试。注意 master 上 `TestMergeProbeTables` 两个测试预存失败（与本改动无关）。

---

## 六、改动清单

| 文件 | 改动 | Fix |
|------|------|-----|
| `internal/server/server.go` | `ChatWrite` 错误路径加 `_ = conn.Close()` | A |
| `internal/daemon/chat.go` | `CloseChat` 补 `e.close()` | C |
| `internal/daemon/chat.go` | `chatEntry` 加 `peer` 字段；`OpenChatForAgent` 设置之；新增 `closeChatsForPeer` | B |
| `internal/daemon/machine_dispatch.go` | `RegisterMachinePeer` 替换旧 peer 时调 `closeChatsForPeer` | B |
| `internal/daemon/chat_close_test.go`（新） | 上述单测 | A/B/C |

**建议交付顺序**：A + C 一个 PR（最小、可独立合入、立即解决用户报告的 5 分钟阻塞）；B 一个 PR（治本，需更充分测试，尤其 peer 身份竞态）。
