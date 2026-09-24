// Package external 实现外部插件 gRPC 协议中「核心侧」的插件加载通道。
//
// 本包与 internal/grpcsrv 是一个协议的两端：
//   - grpcsrv 由核心提供 BotService，供插件反向调用；
//   - 本包作为 gRPC 客户端连接插件进程的 PluginService，把事件投递给插件。
//
// 数据流：
//
//	EventBus --bot.Handler--> 本包 --PluginService.HandleEvent--> 外部插件进程
//	外部插件进程 --BotService.SendMessage--> grpcsrv --> 适配器 --> 平台
//
// 本包只负责协议与生命周期，不做权限、限流等业务判断：这些由核心侧
// grpcsrv 在收到插件回调时统一执行。
package external

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

// Config 是一个外部插件的连接与身份配置。
//
// Name 是核心侧对插件的唯一标识，同时也是 grpcsrv.TokenInfo.Name 的取值，
// 插件反向调用核心时据此确定自身身份与权限。
type Config struct {
	// Name 是插件名（配置键），也是核心侧 Metadata().Name。
	Name string
	// Addr 是插件 PluginService 的监听地址，必填。
	Addr string
	// CoreAddr 是核心 BotService 的监听地址，写入 InitRequest.reply_service_addr。
	CoreAddr string
	// Token 是核心分配的令牌，用于 Init 与插件反向调用核心。
	Token string
	// Timeout 是单次 HandleEvent 的超时，也用作启动阶段 Init 的就绪等待上限；
	// <=0 时取 10s。
	Timeout time.Duration
	// Permissions 是核心授予该插件的权限，写入 InitRequest 并进入元信息。
	Permissions []bot.Permission
	// Settings 是插件配置，序列化后写入 InitRequest.config_json。
	Settings map[string]any
	// TLS 是连接插件时的客户端 TLS 配置，nil 表示明文。
	TLS *tls.Config
}

// Deps 是外部插件通道的外部依赖。
type Deps struct {
	// Bot 是核心的机器人接口实现，保留给插件上下文使用。
	//
	// 本包不直接使用它：事件处理发生在插件进程内，发送也必须经插件反向
	// 调用核心的 BotService，由 grpcsrv 统一落库与执行权限校验。
	Bot bot.BotAPI
	// Logger 是日志器，nil 时使用 slog.Default()。
	Logger *slog.Logger
	// Config 是该插件的配置，保留给插件上下文使用，可为 nil。
	Config *bot.Config
	// HTTPClient 保留给插件上下文使用，可为 nil。
	HTTPClient *http.Client
}

// New 连接并初始化外部插件。
//
// 连接与 Init 会在 cfg.Timeout（默认 10s）窗口内重试：gRPC 的惰性连接加上
// Init 的 WaitForReady，使「插件进程尚未监听」这种瞬时不可用能自动等到插件
// 起来，而不是在 connection refused 上立即失败。因此启动顺序无关——先起核心、
// 先起插件、或编排器同时拉起两侧都能成功。
//
// 超时仍算失败（快速失败语义不变）：错误信息包含插件名与地址，并可区分
// 「等不到插件」（超时）与「插件拒绝初始化」（ok=false 或插件返回的错误）。
// 插件拒绝初始化同样视为失败。
//
// 返回的 Plugin 持有连接的所有权，Close 之后调用方不得再复用该连接。
func New(cfg Config, deps Deps) (*Plugin, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, fmt.Errorf("pluginmgr/external: 插件 %s 的 Addr 不能为空", cfg.Name)
	}

	creds := insecure.NewCredentials()
	if cfg.TLS != nil {
		creds = credentials.NewTLS(cfg.TLS)
	}
	conn, err := grpc.NewClient(target(cfg.Addr), grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("pluginmgr/external: 连接插件 %s (%s): %w", cfg.Name, cfg.Addr, err)
	}

	p, err := NewWithConn(conn, cfg, deps)
	if err != nil {
		// 初始化失败时不接管连接，由本函数负责回收，避免泄漏。
		_ = conn.Close()
		return nil, err
	}
	return p, nil
}

// NewWithConn 复用已有连接构造外部插件，可用于测试或连接池场景。
//
// 与 New 的差别只有连接来源：初始化流程完全一致，且连接所有权同样移交给
// 返回的 Plugin。失败时调用方仍需自行关闭 conn。
func NewWithConn(conn *grpc.ClientConn, cfg Config, deps Deps) (*Plugin, error) {
	if conn == nil {
		return nil, errors.New("pluginmgr/external: conn 不能为空")
	}

	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("plugin", cfg.Name)

	configJSON, err := encodeSettings(cfg.Settings)
	if err != nil {
		return nil, fmt.Errorf("pluginmgr/external: 插件 %s: %w", cfg.Name, err)
	}

	p := &Plugin{
		name:    cfg.Name,
		timeout: cfg.Timeout,
		meta: bot.Metadata{
			Name:        cfg.Name,
			Version:     metaVersion,
			Author:      metaAuthor,
			Description: metaDescription,
			// 复制一份：Metadata() 是公开 API，不能让调用方后续修改自己的
			// 配置切片就改写已构造插件的权限声明。
			Permissions: append([]bot.Permission(nil), cfg.Permissions...),
		},
		client: pluginpb.NewPluginServiceClient(conn),
		conn:   conn,
		logger: logger,
	}
	if p.timeout <= 0 {
		p.timeout = defaultEventTimeout
	}

	// Init 必须同步完成：核心只有在插件确认持有令牌与配置之后，才可以投递事件。
	//
	// WaitForReady(true) 让本次调用在插件尚未监听时排队等待而非立刻失败：
	// 插件进程可能正在启动（甚至是刚被一起拉起），connection refused 属于
	// 瞬时不可用。等待上限由 ctx 的 deadline 决定，超时即报错。
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	resp, err := p.client.Init(ctx, &pluginpb.InitRequest{
		Name:             cfg.Name,
		Version:          metaVersion,
		Author:           metaAuthor,
		Description:      metaDescription,
		Permissions:      permissionStrings(cfg.Permissions),
		ConfigJson:       configJSON,
		Token:            cfg.Token,
		ReplyServiceAddr: cfg.CoreAddr,
	}, grpc.WaitForReady(true))
	if err != nil {
		if ctx.Err() != nil {
			// 超时说明插件始终没起来（或没响应 Init），与「插件拒绝初始化」
			// 是两类问题，错误信息必须能区分，否则排查方向会被带偏。
			return nil, fmt.Errorf("pluginmgr/external: 插件 %s (%s) 在 %s 内未就绪: %w",
				cfg.Name, cfg.Addr, p.timeout, err)
		}
		return nil, fmt.Errorf("pluginmgr/external: 插件 %s (%s) 初始化失败: %w", cfg.Name, cfg.Addr, err)
	}
	if !resp.GetOk() {
		return nil, fmt.Errorf("pluginmgr/external: 插件 %s (%s) 拒绝初始化: %s",
			cfg.Name, cfg.Addr, resp.GetError())
	}

	logger.Info("外部插件已就绪",
		"addr", cfg.Addr,
		"core_addr", cfg.CoreAddr,
		"timeout", p.timeout,
	)
	return p, nil
}

// target 把监听地址包装为 gRPC target 字符串。
//
// 使用 passthrough 解析器：地址已是「主机:端口」形式，无需再做 DNS 服务发现，
// 同时避免 grpc 默认的 dns 解析器对特殊地址（如 Unix socket）的额外处理。
func target(addr string) string {
	if strings.Contains(addr, "://") {
		return addr
	}
	return "passthrough:///" + addr
}

// encodeSettings 把插件配置编码为 JSON 字符串。
//
// nil 映射编码为空串（表示「没有配置」），非 nil 的空映射编码为 "{}"
// （表示「配置为空对象」），两者在插件侧可区分。
func encodeSettings(settings map[string]any) (string, error) {
	if settings == nil {
		return "", nil
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("配置序列化失败: %w", err)
	}
	return string(encoded), nil
}

// permissionStrings 把权限转换为 proto 需要的字符串列表。
func permissionStrings(perms []bot.Permission) []string {
	if len(perms) == 0 {
		return nil
	}
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, string(p))
	}
	return out
}
