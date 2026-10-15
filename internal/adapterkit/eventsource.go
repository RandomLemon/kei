package adapterkit

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/RandomLemon/kei/pkg/bot"
)

// EventSourceOptions 描述一个入站 HTTP 事件源的共享语义：日志前缀、请求体上限、
// 鉴权钩子与解析钩子。适配器只需提供平台特有的鉴权与解析，方法校验、限长读体、
// ACK 顺序与日志文案由本包统一实现。
type EventSourceOptions struct {
	// Name 是日志前缀，用于在共享实现里区分适配器（例如 "onebot"）。
	// 日志文案固定为 "<Name>: <文案>"。
	Name string
	// MaxBodyBytes 是请求体上限；为 0 时取 DefaultMaxBodyBytes。超限与读体失败
	// 一样回 400。
	MaxBodyBytes int64
	// Authorize 校验一个上报请求；为 nil 表示不校验。返回 false 时回 401 并记一条
	// warn（含 remote 与 path），请求体不会被读取。
	Authorize func(*http.Request) bool
	// Parse 把请求体解析为统一事件；为 nil 表示任何请求都解析失败。返回错误时回
	// 400 并记一条 warn（含 remote 与 err）。ctx 为请求的 ctx，实现不得在返回后
	// 继续持有 body。
	Parse func(context.Context, []byte) (*bot.Event, error)
	// Logger 是日志器，为 nil 时使用 slog.Default()。
	Logger *slog.Logger
}

// Handler 返回事件上报的 HTTP 处理函数。
//
// 流程与顺序固定：方法非 POST → 405（带 Allow: POST，不记日志、不读体）→ 鉴权失败
// → 401 → 限长读体失败（含超限）→ 400 → 解析失败 → 400 → 写 204 并 Flush → 同步
// 调用 sink.Emit。先 ACK 再投递保证平台回调快速返回；Emit 在请求 goroutine 内同步
// 调用，因为 bot.EventSink 契约要求实现方快速返回。Emit 失败只记一条 warn，不改变
// 已经发出的 204，也不会重试。
//
// 日志文案固定为 "<Name>: 上报校验失败" / "读取上报请求体失败" / "解析上报事件失败" /
// "投递事件失败"，级别均为 warn。
func (o EventSourceOptions) Handler(sink bot.EventSink) http.HandlerFunc {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "only POST is supported", http.StatusMethodNotAllowed)
			return
		}
		if o.Authorize != nil && !o.Authorize(r) {
			log.Warn(o.Name+": 上报校验失败", "remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, o.maxBodyBytes()))
		if err != nil {
			log.Warn(o.Name+": 读取上报请求体失败", "remote", r.RemoteAddr, "err", err)
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}

		var ev *bot.Event
		if o.Parse != nil {
			ev, err = o.Parse(r.Context(), body)
		} else {
			err = errNoParse
		}
		if err != nil {
			log.Warn(o.Name+": 解析上报事件失败", "remote", r.RemoteAddr, "err", err)
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusNoContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		if err := sink.Emit(r.Context(), ev); err != nil {
			log.Warn(o.Name+": 投递事件失败", "id", ev.ID, "err", err)
		}
	}
}

// maxBodyBytes 返回生效的请求体上限。
func (o EventSourceOptions) maxBodyBytes() int64 {
	if o.MaxBodyBytes > 0 {
		return o.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

// errNoParse 是未提供 Parse 钩子时的解析失败原因。
var errNoParse = errors.New("未配置事件解析函数")

// TokenAuthorizer 返回按固定令牌校验上报请求的鉴权函数。
//
// 令牌为空时返回的函数总是放行，等价于「不校验」；否则要求
// Authorization: Bearer <token> 与 query 参数 access_token=<token> 任一匹配（两者
// 都不匹配才拒绝）。比较使用常数时间算法，避免按字节泄漏令牌。返回的函数可安全地
// 被并发调用。
func TokenAuthorizer(token string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		if token == "" {
			return true
		}
		if got := bearerToken(r.Header.Get("Authorization")); got != "" &&
			subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
			return true
		}
		if got := r.URL.Query().Get("access_token"); got != "" &&
			subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
			return true
		}
		return false
	}
}

// bearerToken 从 Authorization 头中提取 Bearer 令牌。
//
// 前缀比较大小写不敏感（RFC 7235 的 scheme 不区分大小写），令牌本体保留原样，
// 只去掉首尾空白；不含 Bearer 前缀时返回空字符串。
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
