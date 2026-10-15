// Package adapterkit 汇集内置适配器共用的入站 HTTP 事件源实现。
//
// 只收录「已被内置适配器实际使用、且语义必须唯一」的共享件：
//
//   - 事件源服务器与超时默认值：NewServer 构造带默认超时表的 *http.Server；
//   - 入站请求体上限：DefaultMaxBodyBytes；
//   - HTTP 事件源处理器：方法校验、鉴权钩子、限长读体、解析、先 ACK 后投递，
//     见 EventSourceOptions.Handler；
//   - 令牌鉴权：Authorization: Bearer 或 ?access_token=，见 TokenAuthorizer；
//   - 收尾：在调用方给定预算内关闭服务器并把 http.ErrServerClosed 归一为 nil，
//     见 Shutdown。
//
// 明确不抽「有界队列 + worker」这类抽象：入站事件在请求 goroutine 内同步调用
// bot.EventSink.Emit（该接口的契约要求实现方快速返回，返回值只表示「已入队」），
// 内置适配器当前没有任何事件队列，为假想的队列层建抽象不符合仓库「禁止无使用者
// 的抽象」的规则；等到真的出现第二个需要队列的入站形态时再抽不迟。
//
// 本包位于 internal/，第三方适配器不能 import：它们需在自己的包内实现同样的
// 语义，状态码、日志文案与 ACK 顺序即契约，默认值见 docs/adapter.md 6.6 第 11 条。
package adapterkit

import (
	"log/slog"
	"net/http"
	"time"
)

const (
	// DefaultReadHeaderTimeout 是入站事件源的请求头读取超时默认值。
	//
	// 取值理由：webhook 型入口的请求体很小（平台回调 JSON），正常客户端在毫秒级
	// 完成连接与请求头发送，10s 足以吸收网络抖动，又不给慢速发送请求头的攻击留
	// 下长连接。
	DefaultReadHeaderTimeout = 10 * time.Second
	// DefaultReadTimeout 是整请求读取超时默认值（自连接建立起算，含请求体）。
	//
	// 取值理由：上报体积受 DefaultMaxBodyBytes 约束，30s 足够慢速链路把一次上报
	// 发完；再长只会把资源押在异常连接上。
	DefaultReadTimeout = 30 * time.Second
	// DefaultWriteTimeout 是响应写出超时默认值。
	//
	// 取值理由：事件源只回 204/4xx 这类空体或极短响应，30s 已远超正常写出的耗时，
	// 作用是在对端停止读取时尽快回收连接。
	DefaultWriteTimeout = 30 * time.Second
	// DefaultIdleTimeout 是 keep-alive 空闲超时默认值。
	//
	// 取值理由：平台上报通常偶发但可能成簇，60s 让同一平台的连续上报复用连接，
	// 同时避免空闲连接常驻。
	DefaultIdleTimeout = 60 * time.Second
	// DefaultMaxBodyBytes 是单个入站 HTTP 请求体、适配器读取的出站 HTTP 响应体，
	// 以及平台 WebSocket 单帧载荷的字节上限（1 MiB）。
	//
	// 取值理由：webhook 上报是结构固定的 JSON，正常远小于 1 MiB；上限的作用只是
	// 防止异常实现或攻击者打爆内存，因此给得宽松到不会误伤合法上报。
	DefaultMaxBodyBytes = 1 << 20
)

// ServerOptions 是 NewServer 的入参。
type ServerOptions struct {
	// Handler 是事件源的路由；必填，为 nil 时服务器对任何请求回 404。
	Handler http.Handler
	// Logger 是日志器，为 nil 时使用 slog.Default()。net/http 内部的协议错误
	// （握手失败、连接异常等）经 ErrorLog 以 warn 级别写入它，与适配器日志同源，
	// 便于按 bot 排查。
	Logger *slog.Logger
	// ReadHeaderTimeout 覆盖同名的默认值；为 0 时取 DefaultReadHeaderTimeout。
	ReadHeaderTimeout time.Duration
	// ReadTimeout 覆盖同名的默认值；为 0 时取 DefaultReadTimeout。
	ReadTimeout time.Duration
	// WriteTimeout 覆盖同名的默认值；为 0 时取 DefaultWriteTimeout。
	WriteTimeout time.Duration
	// IdleTimeout 覆盖同名的默认值；为 0 时取 DefaultIdleTimeout。
	IdleTimeout time.Duration
}

// NewServer 按 ServerOptions 构造入站 HTTP 事件源的服务器。
//
// 四项超时用默认值填空（见 Default* 常量），ErrorLog 指向 Logger 的 warn 级别
// 日志器：net/http 默认把内部错误写到标准错误，这里改为走适配器的结构化日志。
// 返回值不启动监听，调用方自行 net.Listen + Serve（适配器需要先拿到真实监听地址）
// 并在收尾时调用 Shutdown。
func NewServer(opts ServerOptions) *http.Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &http.Server{
		Handler:           opts.Handler,
		ReadHeaderTimeout: orDefault(opts.ReadHeaderTimeout, DefaultReadHeaderTimeout),
		ReadTimeout:       orDefault(opts.ReadTimeout, DefaultReadTimeout),
		WriteTimeout:      orDefault(opts.WriteTimeout, DefaultWriteTimeout),
		IdleTimeout:       orDefault(opts.IdleTimeout, DefaultIdleTimeout),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// orDefault 在 v 为正时返回 v，否则返回 def。
func orDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}
