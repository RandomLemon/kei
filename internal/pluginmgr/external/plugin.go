package external

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/RandomLemon/kei/internal/grpcsrv"
	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

const (
	// defaultEventTimeout 是 Config.Timeout 未配置时的单次事件处理超时。
	defaultEventTimeout = 10 * time.Second
	// shutdownTimeout 是 Stop 阶段等待插件退出的上限。
	//
	// 使用独立 context：传入的 ctx 可能已经取消（进程正在退出），
	// 但仍应尽力通知插件释放资源。
	shutdownTimeout = 5 * time.Second

	// metaVersion/metaAuthor/metaDescription 是外部插件在核心侧的固定元信息。
	//
	// 真实版本与作者由插件进程自己掌握，核心只能描述「这是一个配置驱动的
	// gRPC 外部插件」，因此不伪装成插件自报的取值。
	metaVersion     = "external"
	metaAuthor      = "config"
	metaDescription = "gRPC 外部插件"
)

// Plugin 是外部插件的核心侧代理，实现 bot.Plugin。
//
// Plugin 本身不处理业务逻辑：Setup 注册的 Handler 只负责把事件经 gRPC 投递给
// 插件进程，插件的回复由其反向调用核心的 BotService 完成。
//
// Plugin 的方法可被多个 goroutine 并发调用；Stop 与 Close 幂等。
type Plugin struct {
	name    string
	timeout time.Duration
	meta    bot.Metadata

	client pluginpb.PluginServiceClient
	conn   *grpc.ClientConn
	logger *slog.Logger

	stopOnce  sync.Once
	closeOnce sync.Once
	closeErr  error
}

// 确保 Plugin 满足 bot.Plugin。
var _ bot.Plugin = (*Plugin)(nil)

// Name 返回插件名，即配置中的插件键。
func (p *Plugin) Name() string { return p.name }

// Metadata 返回核心侧的插件元信息。
//
// 名称与权限来自核心配置，版本/作者/说明是外部插件通道的固定标识。
func (p *Plugin) Metadata() bot.Metadata {
	return p.meta
}

// Setup 注册兜底事件处理器。
//
// 使用 OnAll 而不是更细的规则：事件过滤属于插件进程自己的职责，核心只保证
// 「事件送达插件」，否则插件能力会被核心侧的规则提前裁剪。优先级保持默认 0，
// 以免外部插件抢占或无谓遮挡编译期插件的规则。
func (p *Plugin) Setup(_ context.Context, reg bot.Registrar) error {
	if reg == nil {
		return errors.New("pluginmgr/external: Registrar 不能为空")
	}
	reg.OnAll(p.handleEvent)
	return nil
}

// Start 是空实现：外部插件进程由框架通过 dial 连接，无需核心额外启动动作。
//
// 这里刻意不做 GetConfig 之类的健康检查——那会引入周期性 RPC 并可能与本包
// 的生命周期竞态；插件可用性由每次 HandleEvent 的结果体现。
func (p *Plugin) Start(context.Context) error { return nil }

// Stop 通知插件退出并释放连接。
//
// 即使传入的 ctx 已取消也会尝试 Shutdown，因为「让插件释放资源」比尊重取消
// 更重要；通知失败只记日志，不阻断关闭流程。Stop 可重复调用。
func (p *Plugin) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.stopOnce.Do(func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if _, err := p.client.Shutdown(cctx, &pluginpb.Empty{}); err != nil {
			p.logger.Warn("通知外部插件退出失败", "err", err)
		}
	})
	return p.Close()
}

// Close 关闭底层连接，幂等。
//
// 与 Stop 的差别是不通知插件进程：用于插件已被外部终止、或只需要回收
// 本进程资源（连接与 goroutine）的场景。
func (p *Plugin) Close() error {
	p.closeOnce.Do(func() {
		p.closeErr = p.conn.Close()
	})
	return p.closeErr
}

// handleEvent 把事件转换为 proto 并投递给插件进程。
//
// 失败语义：RPC 错误与插件自报的处理错误都会向上返回，由引擎中间件决定
// 记日志或重试；handled 为 false 表示插件主动放弃该事件，属于正常结果。
func (p *Plugin) handleEvent(ctx context.Context, ev *bot.Event, _ bot.Reply) error {
	req := grpcsrv.EventToProto(ev)
	if req == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	hctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	res, err := p.client.HandleEvent(hctx, req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// 上游已取消/超时，错误由上游自己定义，无需再包装一层。
			return ctxErr
		}
		return fmt.Errorf("pluginmgr/external: 插件 %s 处理事件 %s: %w", p.name, req.GetId(), err)
	}
	if msg := res.GetError(); msg != "" {
		return fmt.Errorf("pluginmgr/external: 插件 %s 处理事件 %s: %s", p.name, req.GetId(), msg)
	}
	return nil
}
