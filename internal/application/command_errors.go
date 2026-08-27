package application

import "errors"

// 应用层公开的错误哨兵供调用方进行稳定分类。
var errCommandConflict = errors.New("命令版本冲突")
