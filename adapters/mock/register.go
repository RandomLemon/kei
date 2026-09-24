package mock

import "github.com/RandomLemon/kei/pkg/bot"

// adapterName 是 Mock 适配器在注册表中的名字，对应配置里的 bots[].adapter: mock。
const adapterName = "mock"

// mockOptions 是 Mock 适配器接受的实例级配置键（bots[] 条目上的私有键）。
var mockOptions = []string{"platform", "listen_addr"}

// init 把 Mock 适配器注册进适配器注册表，与插件一样由空白导入触发、
// 再由核心按配置中的 bots[].adapter 启用；注册只记录工厂，实例状态在
// 工厂产出的 Adapter 上，不在包级变量里。
func init() {
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        adapterName,
		Version:     "v0.1.0",
		Author:      "core",
		Description: "Mock 适配器：不连接真实平台，事件经 Inject 或 HTTP 控制面手动注入，发送结果记录在内存中，用于本地联调与测试。",
		Platforms:   []string{defaultPlatform},
		Permissions: []bot.Permission{bot.PermNetListen},
		Options:     mockOptions,
	}, newFromContext)
}

// newFromContext 依据核心给出的上下文构造 Mock 适配器实例。
//
// Mock 不发起外部网络请求，因此不要求 network 权限，也不使用 HTTPClient。
func newFromContext(ac bot.AdapterContext) (bot.Adapter, error) {
	return New(Options{
		Name:       ac.BotID,
		Platform:   ac.Config.String("platform", defaultPlatform),
		ListenAddr: ac.Config.String("listen_addr", ""),
		Logger:     ac.Logger,
	}), nil
}
