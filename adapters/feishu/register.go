package feishu

import (
	"errors"

	"github.com/RandomLemon/kei/pkg/bot"
)

// feishuOptions 是飞书适配器接受的实例级配置键（bots[] 条目上的私有键）。
var feishuOptions = []string{
	"app_id", "app_secret", "verification_token", "encrypt_key",
	"listen_addr", "path", "base_url",
}

// init 把飞书适配器注册进适配器注册表，与插件一样由空白导入触发、
// 再由核心按配置中的 bots[].adapter 启用；注册只记录工厂，实例状态在
// 工厂产出的 Adapter 上，不在包级变量里。
func init() {
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        PlatformName,
		Version:     "v0.1.0",
		Author:      "core",
		Description: "飞书（Lark）适配器：事件经开放平台事件订阅回调进入，发送经开放平台 IM 接口发出。",
		Platforms:   []string{PlatformName},
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermNetListen},
		Options:     feishuOptions,
	}, newFromContext)
}

// newFromContext 依据核心给出的上下文构造飞书适配器实例。
//
// 适配器声明了 network 权限，核心只有在逐项授予后才会下发 HTTPClient；
// 缺少它说明权限未授予，这里必须直接失败，而不是回落到本包自带的默认客户端。
func newFromContext(ac bot.AdapterContext) (bot.Adapter, error) {
	if ac.HTTPClient == nil {
		return nil, errors.New("feishu: 未声明 network 权限，无法调用平台 API")
	}
	return New(Options{
		Name:              ac.BotID,
		AppID:             ac.Config.String("app_id", ""),
		AppSecret:         ac.Config.String("app_secret", ""),
		VerificationToken: ac.Config.String("verification_token", ""),
		EncryptKey:        ac.Config.String("encrypt_key", ""),
		ListenAddr:        ac.Config.String("listen_addr", ""),
		Path:              ac.Config.String("path", ""),
		BaseURL:           ac.Config.String("base_url", ""),
		HTTPClient:        ac.HTTPClient,
		Logger:            ac.Logger,
	})
}
