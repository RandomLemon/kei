//go:build !linux

package manage

import (
	"context"
	"errors"
)

// hostStatus 在非 Linux 平台不支持主机硬件采集。
//
// 硬件信息来自 /proc 与 /sys（Linux 专有），其它操作系统暂无实现，因此直接
// 返回错误，由 /manage status 渲染为「主机状态采集失败」。
func hostStatus(context.Context) (hostInfo, error) {
	return hostInfo{}, errors.New("当前平台不支持主机硬件采集（仅 Linux）")
}
