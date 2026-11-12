# cpa-plugin-commandcode-quota

CLIProxyAPI 插件:按 **Command Code 账户余额**自动选号,额度用光的账号在调度阶段直接跳过。

与只做"事后标记"的调度器不同,本插件在 `scheduler.pick` 里**先把没钱的 key 从候选集剔除**,再输出选中的 auth id。宿主的请求不会先打到余额耗尽的账号再失败重试 —— 从根本上避免"调用没额度的号报错"。

## 它做什么

- **余额感知选号**:每次请求前读余额快照,剔除任一档(5 小时 / 周 / 月)剩余 ≤ `floor_usd` 的账号
- **零额外延迟**:余额缓存在内存,后台按 `quota_ttl_seconds` 刷新;请求路径只读缓存,仅在缓存过期时同步查一次(带超时)
- **绝不因为查不到余额而断路**:上游失败时沿用旧快照;没有旧值就放行,宁可多用一次也不阻断服务
- **全部耗尽时明确拒绝**:返回 `Reject`,而不是偷偷用一个没钱的号
- **看板**:5 小时 / 周 / 月三档额度、进度条、重置倒计时,用光的账号折叠成一条并露出重置日期
- **原生配额支持**:实现 `quota_provider`,CPA 管理端可直接看到每个凭据的余额

## 安装

只能通过插件商店安装(面板的上传按钮只收认证 JSON,不收 `.so`)。

1. CPA 配置里加插件源:

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - https://raw.githubusercontent.com/yanjie233/cpa-plugin-commandcode-quota/main/registry.json
```

2. 面板 → 插件商店 → 找到 **CommandCode Quota** → 安装
3. 插件管理 → 启用,并按需配置参数
4. 若热重载没生效,重启 CPA 容器一次

## 配置

```yaml
plugins:
  enabled: true
  configs:
    commandcode-quota:
      enabled: true
      priority: 100
      api_base: https://api.commandcode.ai
      floor_usd: 0.1              # 任一档剩余 ≤ 该值即跳过(绝对值,不随套餐漂移)
      quota_ttl_seconds: 60       # 余额快照新鲜期
      timeout_seconds: 5          # 单次查询超时
      providers: []               # 留空接管全部;可填 openai-compatibility
      dry_run: false              # true = 只记录决策,不影响选号
      exclude_exhausted: true     # 关闭则只展示不跳过
      debug_log: false            # 在面板记录每次选号摘要
```

`floor_usd` 用**绝对金额**而不是比例:$10 套餐的 1% 是 $0.10,$300 套餐的 1% 是 $3,比例阈值在不同套餐下含义完全不同。

## 前提

账号必须能以 `openai-compatibility` 的 `api-key-entries` 形式被宿主发现,且 `base-url` 或名称里含 `commandcode`。插件通过 `host.auth.list` / `host.auth.get` 读取 key。

## 行为细节

| 情况 | 行为 |
|---|---|
| 余额充足 | 按瓶颈比例(三档最小剩余率)选最优,并列时比剩余金额 |
| 余额 ≤ `floor_usd` | 从候选剔除,记录原因 |
| 全部候选耗尽 | `Handled:true, Reject:true` |
| 拿不到 key | 保守放行(无法判断) |
| 上游查询失败 | 用旧快照;无旧值则放行 |
| 候选为空 / provider 不匹配 | `Handled:false`,交还宿主内置调度 |
| `dry_run: true` | 只记录,`Handled:false` |

## 面板

`/v0/resource/plugins/commandcode-quota/panel`

挂在宿主的管理鉴权之下,能打开即已认证,插件本身不需要密码。

## 构建

需要 Go >= 1.26.11、gcc、glibc(运行镜像若是 Alpine/musl,`.so` 会 dlopen 失败)。

```bash
./build.sh
```

产物 `dist/commandcode-quota.so`,在 `linux/amd64` 上可直接被 CPA 加载。

## 测试

```bash
go test ./internal/quota/          # 余额解析、缓存、耗尽判定、并发去重
go run ./cmd/realcheck keys.json   # 用真实 key 验证上游连通
```

`tests/so_harness` 会用 cgo 直接 `dlopen` 交付的 `.so`,驱动真实 C ABI 验收(注册、选号、管理路由、畸形输入)。

## License

MIT
