# 家居自动化核心（automation）

一个纯 Go 的确定性家居自动化引擎：接收虚拟/真实设备上报的事件，按规则评估
**持续条件**，调度**延迟动作**，处理**人工覆盖与恢复**，裁决**互相冲突的动作**，
并把状态持久化以便重启续跑。所有裁决都写入决策日志，原始事件历史可离线重放。

## 快速开始

    # 运行全部单元测试
    go test ./...

    # 运行独立演示（虚拟设备，无需任何硬件）
    go run ./cmd/homeauto-demo

    # 用演示录制的旧事件历史做确定性重放
    go run ./cmd/homeauto-demo --replay

演示在固定虚拟时钟上交错推进以下过程，并逐条打印每个动作的执行/取消原因：

1. t+00m 天黑，夜间条件出现；
2. t+05m 条件持续满 5 分钟，排定 3 分钟后开灯；
3. t+07m 用户手动关灯，挂起动作立即取消，规则进入 8 分钟人工覆盖；
4. t+10m 进程重启，从同一状态文件恢复设备状态、覆盖期限与待执行动作；
5. t+15m 覆盖到期，**不会补执行早已失效的旧动作**，而是重新开始观察条件；
6. t+23m 条件重新持续满足后延迟开灯，并联动指示灯（动作生成的状态可触发别的规则）；
7. t+25m 影院模式（更高优先级）裁决关灯，与夜间开灯动作的冲突按优先级定胜负。

## 接入方式

### 1. 创建核并注册规则

    store := automation.FileStore{Path: "var/automation.json"}
    core, err := automation.NewCore(store, time.Now) // store 传 nil 表示纯内存

    err = core.AddRule(automation.Rule{
        ID:        "night-light",
        Priority:  5,
        Condition: automation.DeviceProperty{DeviceID: "daylight", Name: "dark",
            Operator: automation.OpEq, Value: 1},
        Duration:  5 * time.Minute,  // 条件必须连续满足多久才触发
        ManualTTL: 8 * time.Minute,  // 人工覆盖有效期限，0 表示直到条件重新装弹
        Action: automation.SetStateAction{
            DeviceID: "lamp", Name: automation.PoweredOn,
            Value: 1, After: 3 * time.Minute, // 触发后延迟多久落地
        },
    })

### 2. 上报事件 / 人工操作

    sensor := automation.VirtualDevice{ID: "daylight", Types: []string{"light-sensor"}}
    core.Process(sensor.Event("dark", 1, time.Now()))                        // 设备上报
    core.Process(automation.ManualControl("lamp", automation.PoweredOn, 0, time.Now())) // 人工

### 3. 推进时钟

延迟动作不会自己落地，需要由调用方周期性推进时钟（例如每分钟一次）：

    core.Tick(time.Now())

### 4. 读取状态与审计日志

- Snapshot()：当前全部设备属性值及最后来源（设备/人工/自动化）。
- PendingActions()：已排定但尚未到点的延迟动作。
- AppliedActions()：已实际执行的动作流水。
- Decisions()：决策日志，含 executed / scheduled / canceled 与原因。
- History()：原始事件历史。

## 语义说明（验收口径）

### 持续条件

- 条件从“假”变“真”时开始计时；Duration 内条件只要变假一次，计时清零重来。
- 条件持续满足达到 Duration 后规则才触发一次（Fired 锁存）。
- 动作执行后若条件一直为真不会重复触发；条件变假后再次持续满足才可再触发。

### 延迟动作与失效重校验

- Action.After > 0 时动作进入待执行队列，到 CompleteAt 才落地。
- **到点时重新校验**：规则仍被人工覆盖、或条件已不再成立，动作取消
  （原因分别记为 rule_blocked_manual、condition_false）。
- 因此“手动操作结束后直接执行早已失效的自动化动作”不会发生；
  TTL 到期后必须重新看到条件并再次持续满足，才会生成新动作。

### 人工覆盖及恢复

- 对某设备属性的人工操作会：
  1. 立即取消该属性上所有挂起的自动化动作（manual_intervention）；
  2. 对所有控制该属性的规则建立覆盖，覆盖期内不触发。
- ManualTTL == 0（默认）：覆盖持续到条件**重新变假并再次为真**（重新装弹）。
- ManualTTL > 0：覆盖从人工操作时刻起持续该时长，到期自动解除，
  规则从头开始观察条件。

### 多层人工接管（Override 层）

在单属性人工覆盖之上，核支持多层独立的人工接管，可任意叠加：

	// 整层房间接管，10 分钟后自动解除
	core.Takeover(automation.Override{ID: "party-mode",
	    Scope: automation.OverrideScope{Room: "living"},
	    ExpiresAt: &deadline}, at)
	// 同房间某设备的临时例外（无限期，直到显式撤销）
	core.Takeover(automation.Override{ID: "keep-lamp",
	    Scope: automation.OverrideScope{DeviceID: "lamp"}}, at2)
	core.RevokeOverride("keep-lamp", at3)

- 作用范围按 房间 / 设备 / 设备属性 三级限定（OverrideScope），各层
  建立时刻、到期时刻（ExpiresAt，空表示直到显式撤销）完全独立。
- 建立一层时立即取消范围内所有挂起的自动化动作（manual_intervention，
  决策带 OverrideID）；层有效期内罩住的规则不触发。
- 某一层到期或被撤销时，只解除该层：仍被其他层罩住的目标保持人工控制，
  不再被任何层罩住的规则**从头重新观察条件**——恢复的是当前仍有效的
  控制关系，而不是一律回到最早的自动规则。
- 接管层的建立 / 到期 / 撤销都写入决策日志（outcome=override，原因分别为
  override_started / override_expired / override_revoked），并以事件形式
  进入历史，重启恢复与离线重放都能复现。

### 动作组（多步骤序列）

规则的 Steps 非空时，触发的是一组按序执行的步骤，而不是单个动作：

	core.AddRule(automation.Rule{
	    ID: "bedtime-ac", Duration: 2 * time.Minute,
	    Condition: automation.All{Conditions: []automation.Condition{ /* ... */ }},
	    Steps: []automation.Step{
	        // 第一步：确认窗户已关闭（4 分钟内确认不了则记为不确定）
	        {ID: "confirm-window", Verify: &automation.StepVerify{
	            DeviceID: "window", Name: automation.ContactOpen,
	            Operator: automation.OpEq, Value: 0, Within: 4 * time.Minute}},
	        // 第二步：开窗确认后才开空调，并要求设备上报确认
	        {ID: "ac-on", Action: &automation.SetStateAction{
	            DeviceID: "ac", Name: automation.PoweredOn, Value: 1,
	            After: time.Minute}, ConfirmWithin: 3 * time.Minute},
	    },
	})

- 每一步独立区分状态：目标已定义（pending / waiting_confirm）、设备已确认
  效果（confirmed / skipped）、仍不确定（uncertain）、已取消（canceled）；
  整组只有在全部步骤确认后才记录 group_completed——规则触发本身绝不
  把整组记成完成。
- 动作步骤的“确认”要求设备以自身上报（SourceDevice）达到目标值；
  自动化自己写入的值不算设备确认。确认窗口（ConfirmWithin / Verify.Within）
  内未确认时步骤记为 uncertain，后续步骤不再执行。
- 自动下发值不能替代设备回报：重开 / 再次触发后新运行发现自己此前
  下发的目标值时，不当作已满足、也不重复下发已有动作，而是按
  ConfirmWithin 重新等待设备上报（step_waiting_confirm），超时仍只记
  uncertain 并停止依赖它的步骤；等待确认的状态与期限随核持久化，
  迟到确认、再次触发与再次重启都不会重复执行已有动作。
- 准备阶段（确认等待或动作未落地）被人工操作 / 接管层罩住任一剩余步骤
  目标时，整组立即停止，决策记录 group_canceled 及涉及的 OverrideID。
- 接管撤销后规则从头重新观察条件；新运行按当前观察判断每一步：
  目标已满足的步骤记 step_already_satisfied 直接跳过，失去时效的旧动作
  不补发（旧动作 ID 不会再次出现）。
- 多组共享设备时按既有优先级裁决，败方决策的 Detail 记录胜出的动作、
  规则与优先级（等待 / 冲突的依据）；独立设备的组照常推进。
- 组运行状态随核持久化：重启后未完成的确认等待继续核对；停机跨越确认
  窗口时步骤记为 uncertain 并在 Detail 中区分本应截止的时刻与实际发现
  时刻，而不是当作完成。
- 决策与动作都携带 RunID / StepID / OverrideID：每个效果可追溯到哪一次
  规则判断（运行）与哪一层人工覆盖。GroupRuns() 返回各组每步的当前状态。

独立演示（虚拟时钟，无需硬件）交错覆盖：准备阶段接管 → 部分完成后取消 →
撤销后按当前观察重判 → 共享设备冲突裁决 → 重启后继续核对 → 离线设备取消：

    go run ./cmd/homeauto-group-demo
    go run ./cmd/homeauto-group-demo --replay   # 用录制的历史确定性重放

### 设备离线

- 设备上报 online=0（VirtualDevice.Availability(false, at)）即视为离线：
  离线期间其属性对条件不可见（持续条件因此中断计时）；
  指向离线设备的延迟动作到点时取消并记录 device_offline，不会下发。
- 设备恢复在线（online=1）后，人工接管层依然有效，规则按当前状态重新裁决。

### 互相冲突动作的裁决

- 同一设备同一属性上目标值不同的待执行动作视为冲突。
- Priority 数值大者胜；相同优先级按规则 ID 字典序，较小者稳定获胜；
  再相同则按动作 ID 兜底，保证结果确定、可复现。
- 败方动作取消并记录 superseded_by_higher_priority；目标值相同的重复动
  作去重并记录 duplicate_pending，不会重复下发。
- 裁决在动作排定时和到点落地前都会进行，低优先级动作无法抢在高优先级之前落地。

### 重启恢复

- 设备状态、每条规则的持续计时/触发/覆盖状态（含覆盖截止时刻）、
  待执行动作及截止时刻、事件历史与决策日志都通过 FileStore 原子写持久化。
- 重启后重新 NewCore 并注册同一套规则即可续跑；延迟动作按原始截止时刻生效，
  既不会提前也不会因重启被遗忘。
- 重启跨越接管层到期时刻时，到期决策的 At 记录**本应到期的时刻**
  （ExpiresAt），Detail 记录实际发现到期的评估时刻——日志因此区分
  “停机期间应当发生的决策”与“恢复后实际执行的处理”。
- 停机期间错过到点的延迟动作不会一律补发：SetStateAction.ValidFor 限定
  到点后的有效窗口，落后超过该窗口的动作以 stale_action_expired 取消，
  决策 Detail 记录原到点时刻与失效时刻，明确“该动作实际没有执行”。

### 防循环

- 规则动作产生的设备状态可以被其他规则的条件看到（支持联动）。
- 一次 Process / Tick 内按固定顺序做定点评估，每条规则每轮最多触发一次，
  总轮数有上限，因此 A→B→A 的互相触发不会无限循环。

### 历史重放

    replayed, decisions, err := automation.Replay(nil, rules, core.History(), cutoff)

- 仅依赖事件自身时间戳，不依赖实时时钟；重放时随每个事件先推进时钟再处理事件。
- 只重放 cutoff 之前的事件，可重放任意旧历史并得到与当时一致的决策序列。
- 时钟推进（Tick）本身也作为事件进入历史，接管层的建立/撤销同样是历史事件；
  因此重放在与实时运行完全相同的时间点上按相同顺序裁决，
  决策日志逐条一致（见 TestReplayReproducesOverrideDecisions）。

## 条件组合

- DeviceProperty：指定设备的属性比较（eq/ne/lt/le/gt/ge）。
- AnyDeviceProperty：任一具有给定类型的设备满足即可。
- All / Any / Not：条件的与/或/非组合。
