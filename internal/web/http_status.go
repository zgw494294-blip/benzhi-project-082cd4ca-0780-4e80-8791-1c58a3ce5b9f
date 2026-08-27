package web

// 常用 API 状态码集中定义，避免 handler 之间出现语义漂移。
const (
	statusOK       = 200
	statusCreated  = 201
	statusBadInput = 400
)
