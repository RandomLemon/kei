package bot

import (
	"context"
	"sync"
)

// Plugin 是插件接口。
//
// 生命周期：Setup -> Start -> Stop。Setup 在事件处理开始前调用，用于注册
// 命令/正则/关键词/事件规则；Start 用于启动插件自身的后台任务；Stop 必须
// 幂等并释放资源。任何步骤返回错误都会阻止框架启动。
type Plugin interface {
	// Metadata 返回插件元信息，必须包含唯一的 Name。
	Metadata() Metadata
	// Setup 通过 Registrar 注册触发规则。
	Setup(ctx context.Context, reg Registrar) error
	// Start 启动插件后台任务，允许为空实现。
	Start(ctx context.Context) error
	// Stop 停止插件并释放资源。
	Stop(ctx context.Context) error
}

// Metadata 是插件元信息。
type Metadata struct {
	// Name 是插件唯一名称。
	Name string
	// Version 是插件版本。
	Version string
	// Author 是插件作者。
	Author string
	// Description 是插件说明。
	Description string
	// Permissions 是插件申请的权限。
	Permissions []Permission
}

// HasPermission 判断插件是否申请了指定权限。
func (m Metadata) HasPermission(p Permission) bool {
	for _, x := range m.Permissions {
		if x == p || x == PermAll {
			return true
		}
	}
	return false
}

// Permission 是插件权限标识。
type Permission string

// 支持的权限。
const (
	// PermSendMessage 允许插件主动发送消息（而不仅是回复当前会话）。
	PermSendMessage Permission = "send_message"
	// PermReadUser 允许读取用户信息。
	PermReadUser Permission = "read_user"
	// PermNetwork 允许发起外部网络请求。
	PermNetwork Permission = "network"
	// PermStorage 允许读写 Storage。
	PermStorage Permission = "storage"
	// PermAdmin 允许执行管理员命令（配合 Auth 中间件）。
	PermAdmin Permission = "admin"
	// PermAll 表示全部权限，仅内置插件可用。
	PermAll Permission = "*"
)

var (
	registryMu sync.Mutex
	registry   []Plugin
)

// RegisterPlugin 注册编译期插件，通常由插件在 init() 中调用。
//
// 注册只记录实例，实际初始化由引擎按配置的启用列表执行。
func RegisterPlugin(p Plugin) {
	if p == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = append(registry, p)
}

// RegisteredPlugins 返回按注册顺序排列的编译期插件快照。
func RegisteredPlugins() []Plugin {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]Plugin, len(registry))
	copy(out, registry)
	return out
}
