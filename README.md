# auth-fallback

把一组 Cline API key 放在一个 OpenAI 兼容端点后面，429 时自动换号。

调用者只管发请求，密钥由代理挑。

## 一个模型固定用一个 key，429 才换

**默认不换号。** 一个模型会一直用它当前的那个 key，直到这个 key 对它返回 429 —— 这时才把该 key 针对**该模型**冷却，并把这次请求交给下一个 key。

这么设计是为了缓存：上游的 prompt cache 是按账号算的，每个请求都换个 key 等于每次都把缓存打散。会话粘在一个 key 上，命中率才上得去。

换号之后也**不会自动挪回来**。某个 key 冷却结束、重新可用后，它只是回到候选队列里；模型仍待在当前这个能干活的 key 上 —— 挪回去又是一次白白丢弃缓存的切换。只有在当前 key 再次 429 时才继续往后走，走到底就绕回开头。

## 冷却只绑在 (key, 模型) 上

额度是按 key、按模型分开算的，所以冷却也这样记账：

- key 3 的 `deepseek/deepseek-chat` 额度用光了 → 只有「key 3 + deepseek-chat」被停用
- key 3 的 `anthropic/claude-sonnet-4-6` 不受影响：它继续用 key 3，**也不会被拽到别的 key 上**

模型名取自请求体里的 `model` 字段，原样匹配。

## 冷却时间

| 配置 | 默认 | 含义 |
| --- | --- | --- |
| `cooldown` | `60s` | 首次 429 后停用多久 |
| `cooldown_max` | `30m` | 连续 429 时翻倍的上限 |

撞到 429 先停 60s。**等它真的恢复之后又 429**（说明是更长周期的额度，不是每分钟限流），就翻倍：60s → 120s → 240s …… 到 `cooldown_max` 为止。**一旦成功，计数清零**，不会因为历史记录被一直惩罚。

注意翻倍的门槛是「恢复之后又 429」。所有 key 都在冷却时，代理会拿恢复最早的那个再试一次；这种补试失败**不算数**——那个 key 根本还没恢复，说不上是新证据，不该让它的冷却越滚越长。

**为什么默认 60s**：它同时照顾两头。够长，能跨过典型的每分钟限流窗口；够短，不会把只是短暂抖动的 key 闲置太久。8 个 key 意味着最坏情况（全部被限流）也还有 8×60s 的缓冲。真遇到长周期额度，翻倍会自动退避，不需要你调参。

## 只有 429 会换 key

| 上游返回 | 代理行为 |
| --- | --- |
| `429` | 冷却该 key+模型，还有额度就换下一个 key 重试 |
| `200` | 原样透传（含 SSE 流） |
| 其它一切（400/401/403/5xx/网络错误） | 原样返回给调用者 |

其它状态码换 key 没有意义：同一个域名、同一个请求，下一个 key 只会得到同样的答案，白白让调用者多等几轮。

重试**只发生在响应头到达之后、任何字节写给调用者之前**。所以 `stream: true` 的请求也能安全重试，不会吐出半截 SSE。

想要「拿到 429 就直接回给调用者，一次都不重试」，设 `max_attempts: 1`。

用尽 `max_attempts` 之后，代理把**上游自己的那个 429 原样交回**，只是额外附上 `Retry-After`（如果池子知道什么时候有 key 空闲）。代理不会替你编一句「所有 key 都被限流」——`max_attempts` 截断循环时那句话往往是假的，而上游的原文（比如「额度到 14:30 恢复」）比它有用得多。

## 运行

```bash
cp config.example.json config.json && $EDITOR config.json
go build -o auth-fallback . && ./auth-fallback
```

调用者 `base_url` 指到 `http://<host>:8787/v1`。**Authorization 里填什么都可以** —— 代理会丢掉它，换成池子里的 key。

## 配置

```json
{
  "listen": ":8787",
  "upstream": "https://api.cline.bot/api/v1",
  "client_token": "change-me",
  "cooldown": "60s",
  "cooldown_max": "30m",
  "max_attempts": 3,
  "keys": ["sk-...", "sk-..."]
}
```

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `listen` | `:8787` | 环境变量 `PORT` 可覆盖 |
| `upstream` | `https://api.cline.bot/api/v1` | API 基址，不带 `/chat/completions` |
| `client_token` | 空 | 调用者必须出示的 token；留空则不校验（会打警告） |
| `cooldown` | `60s` | 首次 429 冷却 |
| `cooldown_max` | `30m` | 翻倍上限 |
| `max_attempts` | `3` | 单次请求最多用掉几个 key |
| `keys` | 必填 | key 列表 |

时长写 `"90s"` / `"30m"` / `"1h"`，也可以直接写秒数 `90`。

命令行：`-addr` 覆盖监听地址，`-config` 指定配置文件，`-v` 打印每条请求用了哪个 key。

容器里可以用环境变量覆盖文件（env 优先）：`CLINE_KEYS`（逗号分隔）对应 `keys`，`CLIENT_TOKEN` 对应 `client_token`，`PORT` 对应 `listen` 的默认值。只给 `CLINE_KEYS` 而配置文件不存在时，程序按默认值启动——这样镜像里不必放任何密钥。

## 端点

| 端点 | 需要 token | 说明 |
| --- | --- | --- |
| `POST /v1/chat/completions` | 是 | 唯一的转发端点 |
| `GET /status` | 是 | 每个 key 正在服务哪些模型、在冷却哪些模型、还剩多久（key 已打码） |
| `GET /healthz` | 否 | 存活探针，给负载均衡用 |

除了 `/healthz`，所有端点都在 `client_token` 后面——这样以后加端点不会默认敞开。

排查「请求为什么落到这个 key」看 `/status`：

```json
[{"index":0,"key":"sk-cli…9f2a","preferred_for":["anthropic/claude-sonnet-4-6"],
  "cooling":{"deepseek/deepseek-chat":"42s"}},
 {"index":1,"key":"sk-cli…7b1c","preferred_for":["deepseek/deepseek-chat"]}]
```

这里 key 0 正在服务 claude，同时因为 deepseek 被限流而停用 42 秒 —— deepseek 本人已经挪到 key 1 去了。

## 边界

- `client_token` 留空时，**任何能访问到这个端口的人都能花你的额度**。默认监听 `:8787`，暴露到公网前务必设上，或者只绑 `127.0.0.1`。
- `config.json` 里有明文 key，别提交进版本库（`.gitignore` 已排除）。
- 401/403 不做特殊处理，原样透传：一个失效的 key 会继续占着它负责的模型，直到这个模型的下一次请求拿到 401 —— 但 401 不触发换号，所以它会一直卡在那里直到你重启或换掉这个 key。
- 状态在内存里，重启后所有模型回到 key 0。
- 模型名取自请求体，所以内存里那两张表的规模由「调用者用过多少个不同的模型名」决定。冷却表在超过 64 条时会顺手清掉早已过期的条目；**游标表故意不清**——删掉它等于把一个健康的模型悄悄挪回 key 0，正是这套设计要避免的那次切换。
- 请求体上限 64MB。
