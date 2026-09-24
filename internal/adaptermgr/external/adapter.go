package external

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/RandomLemon/kei/internal/grpcsrv"
	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

// instance 是某个 bot 在外部适配器进程中的代理，实现 bot.Adapter。
//
// 实例状态（能力、启动标记、停用标记）只挂在本代理上，因此同一进程服务的
// 多个 bot 互不影响。
type instance struct {
	client   *Client
	botID    string
	settings map[string]any

	mu       sync.RWMutex
	caps     bot.Capabilities
	started  bool
	disabled bool
}

// 确保 instance 满足 bot.Adapter。
var _ bot.Adapter = (*instance)(nil)

// Name 返回平台名；平台在连接建立（Init）时确定，Start 之前也有效。
func (i *instance) Name() string { return i.client.Platform() }

// Capabilities 返回平台能力。
//
// 能力以 Start 应答为唯一权威来源，因此 Start 之前返回零值；引擎只在发送前
// 查询能力，此时实例必然已启动。
func (i *instance) Capabilities() bot.Capabilities {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.caps
}

// Start 通知适配器进程启动该 bot 实例，然后阻塞直到 ctx 结束。
//
// sink 仅用于满足接口：外部适配器通过 BotService.EmitEvent 投递事件，授权与
// 入队都在核心侧完成，本代理不接触 sink。
func (i *instance) Start(ctx context.Context, sink bot.EventSink) error {
	if sink == nil {
		return errors.New("adaptermgr/external: nil sink")
	}
	if err := i.start(ctx); err != nil {
		return err
	}
	i.client.startSupervisor(ctx)

	<-ctx.Done()

	stopCtx, cancel := context.WithTimeout(context.Background(), i.client.rpcTimeout())
	defer cancel()
	if err := i.stop(stopCtx); err != nil {
		i.client.log.Warn("外部适配器实例停止失败", "bot", i.botID, "error", err)
	}
	return nil
}

// Stop 停止该实例；幂等，与 Start 退出路径并发调用是安全的。
func (i *instance) Stop(ctx context.Context) error { return i.stop(ctx) }

// Send 请求适配器进程向平台发送消息。
//
// 平台侧的业务失败（响应的 error 非空）按错误返回，交给引擎的重试与限流逻辑，
// 与进程内适配器的语义一致。
func (i *instance) Send(ctx context.Context, req *bot.SendRequest) (*bot.SendResult, error) {
	if req == nil {
		return nil, errors.New("adaptermgr/external: nil send request")
	}
	if i.isDisabled() {
		return nil, fmt.Errorf("adaptermgr/external: 适配器 %s 的实例 %s 已停用（重连失败）",
			i.client.name, i.botID)
	}

	resp, err := i.client.svc.Send(ctx, &pluginpb.AdapterSendRequest{
		BotId:   i.botID,
		Target:  grpcsrv.TargetToProto(&req.Target),
		Message: grpcsrv.MessageToProto(req.Message),
		ReplyTo: req.ReplyTo,
	})
	if err != nil {
		return nil, fmt.Errorf("外部适配器 %s 的实例 %s 发送失败: %w", i.client.name, i.botID, err)
	}
	if msg := resp.GetError(); msg != "" {
		return nil, fmt.Errorf("外部适配器 %s 的实例 %s 发送失败: %s", i.client.name, i.botID, msg)
	}
	return &bot.SendResult{MessageID: resp.GetMessageId()}, nil
}

// start 执行一次 Start RPC，并把应答中的能力声明落到本实例。
//
// 已启动的实例直接返回 nil，使重连路径可以安全地重复调用。
func (i *instance) start(ctx context.Context) error {
	i.mu.RLock()
	started := i.started
	i.mu.RUnlock()
	if started {
		return nil
	}

	resp, err := i.client.svc.Start(ctx, &pluginpb.AdapterInstance{
		BotId:      i.botID,
		Platform:   i.Name(),
		ConfigJson: encodeSettingsJSON(i.settings),
	})
	if err != nil {
		return fmt.Errorf("启动外部适配器 %s 的实例 %s: %w", i.client.name, i.botID, err)
	}
	if !resp.GetOk() {
		return fmt.Errorf("外部适配器 %s 拒绝启动实例 %s: %s", i.client.name, i.botID, resp.GetError())
	}
	if p := resp.GetPlatform(); p != "" && p != i.Name() {
		return fmt.Errorf("外部适配器 %s 的实例 %s 上报平台 %q，与 %q 不一致",
			i.client.name, i.botID, p, i.Name())
	}

	i.mu.Lock()
	caps := capsFromProto(resp.GetCapabilities())
	missingCaps := caps == (bot.Capabilities{})
	if missingCaps {
		// 全零能力声明无法表达「什么都不支持」：按仅文本处理，避免把文本段
		// 也降级成占位符。core 侧统一在 6.4 的降级规则里依赖这个兜底。
		caps = bot.Capabilities{Text: true}
	}
	i.caps = caps
	i.started = true
	i.disabled = false
	i.mu.Unlock()

	if missingCaps {
		i.client.log.Warn("外部适配器未声明实例能力，按仅文本处理", "bot", i.botID)
	}
	return nil
}

// stop 执行一次 Stop RPC；已停止的实例直接返回 nil。
func (i *instance) stop(ctx context.Context) error {
	i.mu.Lock()
	if !i.started {
		i.mu.Unlock()
		return nil
	}
	i.started = false
	i.mu.Unlock()

	if _, err := i.client.svc.Stop(ctx, &pluginpb.AdapterStopRequest{BotId: i.botID}); err != nil {
		return fmt.Errorf("停止外部适配器 %s 的实例 %s: %w", i.client.name, i.botID, err)
	}
	return nil
}

// startedFlag 返回实例是否处于已启动状态。
func (i *instance) startedFlag() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.started
}

// resetStarted 清除启动标记，供重连路径重新下发 Start。
func (i *instance) resetStarted() {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
}

// isDisabled 返回实例是否因重连失败被停用。
func (i *instance) isDisabled() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.disabled
}

// setDisabled 设置停用标记。
func (i *instance) setDisabled(disabled bool) {
	i.mu.Lock()
	i.disabled = disabled
	i.mu.Unlock()
}

// capsFromProto 把 proto 能力声明转换为 bot.Capabilities。
func capsFromProto(c *pluginpb.Capabilities) bot.Capabilities {
	if c == nil {
		return bot.Capabilities{}
	}
	return bot.Capabilities{
		Text:     c.GetText(),
		Markdown: c.GetMarkdown(),
		Image:    c.GetImage(),
		At:       c.GetAt(),
		Card:     c.GetCard(),
		File:     c.GetFile(),
		Reply:    c.GetReply(),
		Private:  c.GetPrivate(),
		Group:    c.GetGroup(),
	}
}
