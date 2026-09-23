package engine

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/RandomLemon/kei/internal/storage"
	"github.com/RandomLemon/kei/pkg/bot"
)

// pluginAPI 是插件专属的 BotAPI 门面：权限校验与日志字段在这里收口，
// 插件拿不到未经包装的引擎实例。
type pluginAPI struct {
	eng   *Engine
	meta  bot.Metadata
	log   *slog.Logger
	store bot.Storage
}

// 确保 pluginAPI 满足 bot.BotAPI。
var _ bot.BotAPI = (*pluginAPI)(nil)

// pluginAPI 为插件名与元信息构造专属门面，供 pluginmgr 注入 PluginContext。
func (e *Engine) pluginAPI(name string, meta bot.Metadata) bot.BotAPI {
	store := storage.Denied()
	if meta.HasPermission(bot.PermStorage) {
		store = e.store
	}
	return &pluginAPI{
		eng:   e,
		meta:  meta,
		log:   e.log.With("plugin", name),
		store: store,
	}
}

// Send 主动发送消息，需要 PermSendMessage 权限。
func (p *pluginAPI) Send(ctx context.Context, target bot.Target, msg *bot.Message) (*bot.SendResult, error) {
	if !p.meta.HasPermission(bot.PermSendMessage) {
		return nil, fmt.Errorf("engine: plugin %s lacks permission %s", p.meta.Name, bot.PermSendMessage)
	}
	return p.eng.Send(ctx, target, msg)
}

// Reply 回复当前事件所在会话，所有插件都可以使用。
func (p *pluginAPI) Reply(ctx context.Context, ev *bot.Event, msg *bot.Message) (*bot.SendResult, error) {
	return p.eng.Reply(ctx, ev, msg)
}

// Logger 返回带 plugin 字段的日志器。
func (p *pluginAPI) Logger() *slog.Logger { return p.log }

// Storage 返回插件存储；未声明 PermStorage 的插件拿到的是拒绝访问的实现。
func (p *pluginAPI) Storage() bot.Storage { return p.store }
