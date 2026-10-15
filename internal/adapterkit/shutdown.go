package adapterkit

import (
	"context"
	"errors"
	"net/http"
)

// Shutdown 在调用方给定的预算内优雅关闭 srv：停止监听并等在途请求结束。
//
// srv 为 nil 时返回 nil，因此「本次未监听端口」的适配器无需分支；已经在别处关闭
// 过的服务器返回的 http.ErrServerClosed 归一为 nil（关闭是幂等的目标状态）。
//
// deadline 由调用方负责：本函数不自行设超时，ctx 到期即以 ctx 的错误返回，由调用
// 方决定这次收尾占多少预算（OneBot 的阶段划分见其 Stop：http 阶段拿
// httpShutdownTimeout 子预算，剩余时间留给 WebSocket 收尾）。
func Shutdown(ctx context.Context, srv *http.Server) error {
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil && errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
