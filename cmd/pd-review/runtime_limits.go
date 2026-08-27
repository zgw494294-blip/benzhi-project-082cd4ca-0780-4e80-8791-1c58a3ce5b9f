package main

// HTTP 服务的有界超时集中定义，避免入口与自检流程出现不一致。
const (
	serverReadHeaderTimeoutSeconds = 5
	serverReadTimeoutSeconds       = 15
	serverWriteTimeoutSeconds      = 30
)
