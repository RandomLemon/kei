// Package myim 是一个独立 module 形态的第三方适配器示例：它只依赖
// github.com/RandomLemon/kei/pkg/bot，不依赖任何 internal 包，核心仓库
// 不需要为它改动任何文件。
//
// 接入只需两步：
//
//  1. 主程序空导入本包（或本 module）：
//     import _ "github.com/example/kei-adapter-myim"
//  2. 配置里把某个 bot 指向注册名 myim：
//     bots:
//     - name: myim-main
//     adapter: myim
//     api_base: https://im.example.com
//     listen_addr: 127.0.0.1:19081
//     path: /myim/callback
//     self_id: "10001"
//
// 本包在 init() 中调用 bot.RegisterAdapter，注册的是工厂而不是单例：
// 配置里每个 adapter: myim 的 bot 都会调用一次工厂，得到互相隔离的实例。
package myim

import (
	"errors"

	"github.com/RandomLemon/kei/pkg/bot"
)

// adapterName 是注册名，对应配置里的 bots[].adapter: myim。
const adapterName = "myim"

// adapterOptions 是适配器接受的实例级私有配置键，与 Options 的字段一一对应。
var adapterOptions = []string{"api_base", "listen_addr", "path", "self_id"}

// init 把适配器工厂注册进注册表；注册只记录元信息与工厂，实例在核心
// 按配置装配时才构造，因此包内没有可变状态。
func init() {
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        adapterName,
		Version:     "v0.1.0",
		Author:      "third-party",
		Description: "MyIM 平台适配器示例：HTTP 回调接收事件、开放接口发送消息，只依赖公开 SDK pkg/bot。",
		Platforms:   []string{platformName},
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermNetListen},
		Options:     adapterOptions,
	}, New)
}

// New 依据核心给出的上下文构造适配器实例，每个 bot 配置调用一次。
//
// 校验规则与权限声明一致：声明了 PermNetwork 才能拿到 HTTPClient，声明了
// PermNetListen 才允许出现保留键 listen_addr；依赖缺失时返回错误而不是回落到
// 自带默认值，避免配置问题被静默掩盖。
func New(ac bot.AdapterContext) (bot.Adapter, error) {
	if ac.HTTPClient == nil {
		return nil, errors.New("myim: 未获得 HTTPClient，请在元信息中声明 network 权限")
	}
	return NewFromOptions(Options{
		BotID:      ac.BotID,
		APIBase:    ac.Config.String("api_base", ""),
		ListenAddr: ac.Config.String(bot.OptListenAddr, ""),
		Path:       ac.Config.String("path", ""),
		SelfID:     ac.Config.String("self_id", ""),
		HTTPClient: ac.HTTPClient,
		Logger:     ac.Logger,
	})
}
