package onebot

import (
	"errors"

	"github.com/RandomLemon/kei/pkg/bot"
)

// onebotOptions 是 OneBot 适配器接受的实例级配置键（bots[] 条目上的私有键）。
var onebotOptions = []string{
	"mode", "api_url", "ws_url", "listen_addr", "path", "ws_path",
	"ping_interval", "secret", "access_token", "self_id",
}

// init 把 OneBot 适配器注册进适配器注册表，与插件一样由空白导入触发、
// 再由核心按配置中的 bots[].adapter 启用；注册只记录工厂，实例状态在
// 工厂产出的 Adapter 上，不在包级变量里。
func init() {
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        platformName,
		Version:     "v0.3.0",
		Author:      "core",
		Description: "OneBot v11 适配器：按 mode 选择 HTTP 双向 / 正向 WebSocket / 反向 WebSocket，事件入站与发送通道随接入方式切换。",
		Platforms:   []string{platformName},
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermNetListen},
		Options:     onebotOptions,
	}, newFromContext)
}

// newFromContext 依据核心给出的上下文构造 OneBot 适配器实例。
//
// 适配器声明了 network 权限，核心只有在逐项授予后才会下发 HTTPClient；
// 缺少它说明权限未授予，这里必须直接失败，而不是回落到本包自带的默认客户端。
// 反向 WebSocket 不需要出站请求，但 HTTP API、正向 WebSocket 与默认客户端仍需
// network 权限，因此这里保持一致的失败语义。
func newFromContext(ac bot.AdapterContext) (bot.Adapter, error) {
	if ac.HTTPClient == nil {
		return nil, errors.New("onebot: 未声明 network 权限，无法调用平台 API")
	}
	return New(Options{
		Name:         ac.BotID,
		Mode:         ac.Config.String("mode", ""),
		WSURL:        ac.Config.String("ws_url", ""),
		APIURL:       ac.Config.String("api_url", ""),
		ListenAddr:   ac.Config.String("listen_addr", ""),
		Path:         ac.Config.String("path", ""),
		WSPath:       ac.Config.String("ws_path", ""),
		PingInterval: ac.Config.Duration("ping_interval", 0),
		Secret:       ac.Config.String("secret", ""),
		AccessToken:  ac.Config.String("access_token", ""),
		SelfID:       ac.Config.String("self_id", ""),
		HTTPClient:   ac.HTTPClient,
		Logger:       ac.Logger,
	})
}
