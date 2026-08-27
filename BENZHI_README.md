# BENZHI_README

基于 Go 实现的高压电缆局部放电试验放行工作台 Web 项目，一款后端服务，已完整实现高压电缆局部放电试验放行工作台，覆盖三相证据采集、确定性风险评估、异常复验、专家审核、证据冻结、凭据签发与验证。

## 项目说明
- 项目：benzhi-project-082cd4ca-0780-4e80-8791-1c58a3ce5b9f
- 项目用途：已完整实现高压电缆局部放电试验放行工作台，覆盖三相证据采集、确定性风险评估、异常复验、专家审核、证据冻结、凭据签发与验证。
- Go 工具链：`golang:1.22`
- 前端工具链：原生 HTML、CSS 和 JavaScript，由 Go 服务直接提供

## 标准构建、运行和测试命令
进入容器后执行：
cd '/app' && GOTOOLCHAIN=local go build ./...
cd /app && GOTOOLCHAIN=local go run ./cmd/pd-review --self-check -addr=127.0.0.1:19081
cd '/app' && GOTOOLCHAIN=local go test ./...

## Docker 构建和进入容器
chmod +x build_benzhi_docker.sh
./build_benzhi_docker.sh benzhi-project-082cd4ca-0780-4e80-8791-1c58a3ce5b9f-amd64 linux/amd64
./build_benzhi_docker.sh benzhi-project-082cd4ca-0780-4e80-8791-1c58a3ce5b9f-arm64 linux/arm64
docker run -it benzhi-project-082cd4ca-0780-4e80-8791-1c58a3ce5b9f-amd64:latest

## 题目验证命令
1. 预期退出码 0：`go test ./...`
2. 预期退出码 0：`go run ./cmd/pd-review --self-check -addr=127.0.0.1:19081`
