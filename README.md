# 高压电缆局部放电试验放行工作台

本项目面向局部放电试验员、绝缘诊断专家和质量放行负责人，将高压电缆试验任务沿一条可审计状态链推进：任务建档、三相测量采集、确定性风险评估、异常复验、专家审核、证据冻结、放行凭据签发与验证。

工作台由 Go 服务直接提供响应式 HTML、CSS、JavaScript 页面和同源 JSON API，不需要 Node 构建链。业务数据保存在本地 JSON Lines 事件账本中，每个事件包含连续序号、前序摘要和 SHA-256 摘要；投影快照以原子替换方式同步，进程重启时会校验摘要链并重放任务状态。

## 构建与运行

要求 Go 1.22 或更高版本。

```text
go build ./cmd/pd-review
go run ./cmd/pd-review -addr=127.0.0.1:19081 -data-dir=data
```

默认监听 `127.0.0.1:19081`，默认数据目录为 `data`。也可以只设置端口号，例如设置 `PORT=19123` 后运行 `go run ./cmd/pd-review`，服务会监听 `127.0.0.1:19123`。显式 `-addr` 优先于 `PORT`。浏览器打开 `http://127.0.0.1:19081/` 即可进入工作台。

进程收到 `SIGINT` 或 `SIGTERM` 后会停止接收新请求、刷新账本与快照，并有序退出。

## 业务流程

1. 建立任务并登记电缆段、绝缘结构、额定电压、试验方案和责任人。
2. 一次提交 A、B、C 三相测点的峰值、重复率、噪声基线和波形摘要。
3. 运行峰值、重复率、信噪比及相位集中度规则，生成稳定排序的规则命中项。
4. 对异常问题登记替代传感器或复测条件，系统比较前后数据并关闭或升级问题。
5. 专家提交逐项意见、审核结论和复核签名；未解决问题不能通过审核。
6. 质量负责人冻结规范化证据摘要和任务版本，再签发 `PD1` 离线格式放行凭据。
7. 凭据验证先检查离线格式与校验码，再与本机账本中的冻结证据核对。

所有变更命令都要求 `expectedVersion` 乐观并发版本和 `Idempotency-Key` 请求头。版本过期时 API 返回 `409`、错误码 `version_conflict` 和当前版本；同一幂等键重试会复用首次命令结果。

## 测试与自检

运行全部单元与 HTTP 回归测试：

```text
go test ./...
```

运行有界端到端自检：

```text
go run ./cmd/pd-review --self-check -addr=127.0.0.1:19081
```

自检会在指定地址真实启动 HTTP 服务，并在临时数据目录中完成建档、三相采集、异常评估、复验关闭、专家审核、证据冻结、凭据签发、验证和重启重放，然后自动关闭服务并删除临时数据。

## 主要 API

- `GET /`：浏览器工作台。
- `GET /api/campaigns`、`GET /api/campaigns/{id}`：任务列表与完整证据详情。
- `POST /api/campaigns`：任务建档。
- `POST /api/campaigns/{id}/measurements`：提交完整三相测量。
- `POST /api/campaigns/{id}/assessments`：执行确定性风险评估。
- `POST /api/campaigns/{id}/retests`：登记复验并比较结果。
- `POST /api/campaigns/{id}/review`：提交专家审核。
- `POST /api/campaigns/{id}/freeze`：冻结证据和任务版本。
- `POST /api/campaigns/{id}/credential`：签发放行凭据。
- `POST /api/credentials/verify`：验证凭据格式及账本有效性。
