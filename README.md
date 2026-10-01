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

### 多层人工接管

除单条规则的 ManualTTL 覆盖外，还可以通过接管层叠加多路人工接管，
每层有独立的作用范围与到期时间：

    // 整个客厅接管 30 分钟
    roomID, _ := core.BeginTakeover(automation.Takeover{Room: "living", TTL: 30 * time.Minute}, at)
    // 同时某台设备的某个属性再挂一个临时例外，直到显式撤销
    excID, _ := core.BeginTakeover(automation.Takeover{DeviceID: "lamp", Name: automation.PoweredOn}, at)
    core.ReleaseTakeover(excID, later) // 只解除该层，房间接管继续有效

- 作用范围：Room（房间内全部设备）、DeviceID（整台设备）或
  DeviceID+Name（单个属性的临时例外），设备的房间由设备事件携带的 Room 确定。
- 各层独立计时：TTL > 0 到期自动解除（takeover_expired），也可
  ReleaseTakeover 显式撤销（takeover_released）。
- 一层结束只解除该层约束：只要还有任何一层覆盖某规则，该规则仍被接管；
  全部覆盖结束后规则从当前时刻**重新观察条件**，而不是回到最早的自动状态。
- 接管建立时立即取消被覆盖的挂起动作（manual_intervention），
  并让被覆盖规则的持续计时清零。
- 接管以事件形式进入历史（takeover_begin / takeover_end），
  因此重启恢复与离线重放会复现完全相同的接管过程。

### 重启跨越接管到期

- 接管层、待执行动作、设备状态全部持久化；进程停机期间到期的接管层在
  重启后第一次推进时钟时解除，并记录 takeover_expired 决策。
- 停机期间到点的延迟动作**不会按旧时刻补发**：重启后处理时重新校验
  （覆盖、条件、裁决、设备在线），校验失败即取消。
- 这类“迟到”的决策在日志中带 DueAt 字段：DueAt 是动作本应落地的时刻，
  At 是实际处理时刻——借此区分“停机期间应当发生的决策”与
  “实际执行了的动作”。

### 设备离线

- 设备通过 online 属性上报在线状态（VirtualDevice.Status）。
- 动作到点时目标设备处于离线：动作不落地，取消并记录 device_offline，
  历史因此能解释最终为什么没有执行该动作。

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

### 防循环

- 规则动作产生的设备状态可以被其他规则的条件看到（支持联动）。
- 一次 Process / Tick 内按固定顺序做定点评估，每条规则每轮最多触发一次，
  总轮数有上限，因此 A→B→A 的互相触发不会无限循环。

### 历史重放

    replayed, decisions, err := automation.Replay(nil, rules, core.History(), cutoff)

- 仅依赖事件自身时间戳，不依赖实时时钟；重放时随每个事件先推进时钟再处理事件。
- 只重放 cutoff 之前的事件，可重放任意旧历史并得到与当时一致的决策序列。
- 规则竞争、多层接管、设备离线与延迟裁决按同一确定性顺序复现，
  重放决策序列与实时运行完全一致。

## 条件组合

- DeviceProperty：指定设备的属性比较（eq/ne/lt/le/gt/ge）。
- AnyDeviceProperty：任一具有给定类型的设备满足即可。
- All / Any / Not：条件的与/或/非组合。
