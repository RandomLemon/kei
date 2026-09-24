// Package external 实现外部适配器 gRPC 协议中「核心侧」的客户端。
//
// 拓扑与外部插件对称：核心作为 BotService 的 Server（internal/grpcsrv），并主动
// dial 适配器进程的 AdapterService。适配器进程通过 BotService.EmitEvent 把平台
// 事件投递给核心，因此本包不需要（也不能）把事件 sink 交给对端。
//
// 一个适配器进程可服务多个 bot：Init 一次，Start 每个绑定的 bot 一次，Stop 幂等。
// 本包只做协议、权限交集与重连；业务判断由 internal/grpcsrv 与 internal/adaptermgr
// 负责。
package external

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

// 默认参数。
const (
	// defaultTimeout 是单次 RPC 超时与 Init 就绪等待上限。
	defaultTimeout = 10 * time.Second
	// reconnectPollInterval 是连接状态巡检间隔。
	reconnectPollInterval = 500 * time.Millisecond
	// reconnectInitialBackoff 是重连失败的初始退避。
	reconnectInitialBackoff = 100 * time.Millisecond
	// reconnectMaxBackoff 是退避上限。
	reconnectMaxBackoff = 5 * time.Second
	// reconnectMaxAttempts 是连续失败上限，超过后停用实例并转入低速巡检。
	reconnectMaxAttempts = 6
	// reconnectDisabledPoll 是停用后的低速巡检间隔。
	reconnectDisabledPoll = 30 * time.Second
)

// Config 是一个外部适配器的接入配置。
type Config struct {
	// Name 是适配器在配置中的名字（adapters.<name>），也是核心侧的唯一标识。
	Name string
	// Addr 是适配器 AdapterService 的监听地址，必填。
	Addr string
	// CoreAddr 是核心 BotService 的监听地址，写入 InitRequest 供适配器反向调用。
	CoreAddr string
	// Token 是核心分配的令牌，适配器反向调用时携带。
	Token string
	// Timeout 是单次 RPC 超时，也用作启动就绪等待上限；<=0 时取 10s。
	Timeout time.Duration
	// Permissions 是核心授予该适配器的权限，与适配器上报的权限取交集。
	Permissions []bot.Permission
	// Platform 是指定的平台名；适配器上报多个平台时必填。
	Platform string
	// Settings 是进程级配置，序列化后写入 InitRequest.config_json。
	Settings map[string]any
	// TLS 是连接适配器时的客户端 TLS 配置，nil 表示明文。
	TLS *tls.Config
}

// Deps 是外部适配器通道的依赖。
type Deps struct {
	// Logger 是日志器，nil 时使用 slog.Default()。
	Logger *slog.Logger
}

// Client 是一个外部适配器进程的连接与身份，可服务多个 bot 实例。
//
// Client 持有连接的所有权；Close 之后不得再使用。
type Client struct {
	name          string
	addr          string
	token         string
	coreAddr      string
	timeout       time.Duration
	granted       []bot.Permission
	explicit      string // 配置指定的平台名，可为空
	settings      map[string]any
	log           *slog.Logger
	conn          *grpc.ClientConn
	svc           pluginpb.AdapterServiceClient
	superviseOnce sync.Once

	mu        sync.RWMutex
	meta      bot.AdapterMetadata
	platform  string
	instances map[string]*instance
	closed    bool
}

// New 连接并初始化外部适配器。
//
// Init 在 Timeout（默认 10s）窗口内使用 WaitForReady 重试，因此适配器进程尚未
// 监听（或与核心同时启动）不会立即失败；超时仍算失败，错误信息包含名字与地址，
// 并能区分「等不到适配器」与「适配器拒绝初始化」。
//
// 注意时序：核心的 BotService 在适配器装配之后才开始监听，因此适配器只能在
// Start 之后反向调用核心（EmitEvent / GetConfig / Log）；Init 阶段只保存 core_addr。
func New(ctx context.Context, cfg Config, deps Deps) (*Client, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("adaptermgr/external: Addr 不能为空")
	}

	creds := insecure.NewCredentials()
	if cfg.TLS != nil {
		creds = credentials.NewTLS(cfg.TLS)
	}
	conn, err := grpc.NewClient(target(cfg.Addr), grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("adaptermgr/external: 连接适配器 %s (%s): %w", cfg.Name, cfg.Addr, err)
	}

	c, err := newWithConn(ctx, conn, cfg, deps)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// newWithConn 复用已有连接构造客户端，供 New 与 bufconn 测试使用。
func newWithConn(ctx context.Context, conn *grpc.ClientConn, cfg Config, deps Deps) (*Client, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	settingsJSON, err := encodeSettings(cfg.Settings)
	if err != nil {
		return nil, fmt.Errorf("adaptermgr/external: 适配器 %s: %w", cfg.Name, err)
	}

	c := &Client{
		name:      cfg.Name,
		addr:      cfg.Addr,
		token:     cfg.Token,
		coreAddr:  cfg.CoreAddr,
		timeout:   cfg.Timeout,
		granted:   append([]bot.Permission(nil), cfg.Permissions...),
		explicit:  cfg.Platform,
		settings:  cfg.Settings,
		log:       logger.With("adapter", cfg.Name),
		conn:      conn,
		svc:       pluginpb.NewAdapterServiceClient(conn),
		instances: make(map[string]*instance),
	}
	if c.timeout <= 0 {
		c.timeout = defaultTimeout
	}
	if err := c.init(ctx); err != nil {
		return nil, err
	}
	c.log.Info("external adapter ready",
		"addr", cfg.Addr,
		"core_addr", cfg.CoreAddr,
		"platform", c.Platform(),
		"permissions", permissionStrings(c.Metadata().Permissions),
		"settings_json", settingsJSON,
	)
	return c, nil
}

// Metadata 返回适配器上报并生效的元信息（权限已与配置取交集）。
func (c *Client) Metadata() bot.AdapterMetadata {
	c.mu.RLock()
	defer c.mu.RUnlock()
	meta := c.meta
	meta.Platforms = append([]string(nil), meta.Platforms...)
	meta.Permissions = append([]bot.Permission(nil), meta.Permissions...)
	meta.Options = append([]string(nil), meta.Options...)
	return meta
}

// Platform 返回该适配器实例写入 Event.Platform 的平台名。
func (c *Client) Platform() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.platform
}

// Instance 为一个 bot 建立适配器代理。
//
// 实例级配置只在此处校验保留键（net_listen），真正的 Start 由代理的
// Start 触发，与进程内适配器语义一致。
func (c *Client) Instance(botID string, cfg *bot.Config) (bot.Adapter, error) {
	if strings.TrimSpace(botID) == "" {
		return nil, errors.New("adaptermgr/external: botID 不能为空")
	}
	c.mu.RLock()
	closed, meta := c.closed, c.meta
	c.mu.RUnlock()
	if closed {
		return nil, fmt.Errorf("adaptermgr/external: 适配器 %s 已关闭", c.name)
	}

	settings := cfg.Raw()
	if err := checkReservedOptions(botID, settings, c.name, meta.Permissions); err != nil {
		return nil, err
	}

	inst := &instance{client: c, botID: botID, settings: settings}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("adaptermgr/external: 适配器 %s 已关闭", c.name)
	}
	if _, dup := c.instances[botID]; dup {
		c.mu.Unlock()
		return nil, fmt.Errorf("adaptermgr/external: 适配器 %s 的实例 %s 重复", c.name, botID)
	}
	c.instances[botID] = inst
	c.mu.Unlock()
	return inst, nil
}

// Close 停止所有实例、通知适配器进程退出并关闭连接，可重复调用。
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	instances := make([]*instance, 0, len(c.instances))
	for _, inst := range c.instances {
		instances = append(instances, inst)
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.rpcTimeout())
	defer cancel()

	var errs []error
	for _, inst := range instances {
		if err := inst.stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := c.svc.Shutdown(ctx, &pluginpb.Empty{}); err != nil && ctx.Err() == nil {
		errs = append(errs, fmt.Errorf("通知适配器 %s 退出: %w", c.name, err))
	}
	if err := c.conn.Close(); err != nil {
		errs = append(errs, fmt.Errorf("关闭适配器 %s 连接: %w", c.name, err))
	}
	return errors.Join(errs...)
}

// init 执行一次 Init：下发身份、令牌、核心地址与进程级配置，并校验上报的元信息。
func (c *Client) init(ctx context.Context) error {
	initCtx, cancel := context.WithTimeout(ctx, c.rpcTimeout())
	defer cancel()

	resp, err := c.svc.Init(initCtx, &pluginpb.AdapterInitRequest{
		Info: &pluginpb.AdapterInfo{
			Name:        c.name,
			Platforms:   platformsOrNil(c.explicit),
			Permissions: permissionStrings(c.granted),
		},
		Token:      c.token,
		CoreAddr:   c.coreAddr,
		ConfigJson: encodeSettingsJSON(c.settings),
	}, grpc.WaitForReady(true))
	if err != nil {
		if initCtx.Err() != nil {
			return fmt.Errorf("adaptermgr/external: 适配器 %s (%s) 在 %s 内未就绪: %w",
				c.name, c.addr, c.rpcTimeout(), err)
		}
		return fmt.Errorf("adaptermgr/external: 适配器 %s (%s) 初始化失败: %w", c.name, c.addr, err)
	}
	if !resp.GetOk() {
		return fmt.Errorf("adaptermgr/external: 适配器 %s (%s) 拒绝初始化: %s",
			c.name, c.addr, resp.GetError())
	}

	info := resp.GetInfo()
	if info == nil {
		return fmt.Errorf("adaptermgr/external: 适配器 %s 未上报元信息", c.name)
	}
	if name := info.GetName(); name != "" && name != c.name {
		return fmt.Errorf("adaptermgr/external: 适配器上报名 %q 与配置名 %q 不一致", name, c.name)
	}
	platforms := info.GetPlatforms()
	if len(platforms) == 0 {
		return fmt.Errorf("adaptermgr/external: 适配器 %s 未上报 Platforms", c.name)
	}
	platform, err := resolvePlatform(c.name, c.explicit, platforms)
	if err != nil {
		return err
	}

	declared := permissionsFromStrings(info.GetPermissions())
	if hasPermission(declared, bot.PermAll) {
		return fmt.Errorf("adaptermgr/external: 适配器 %s 不得声明 %s 权限（仅插件可用）", c.name, bot.PermAll)
	}

	meta := bot.AdapterMetadata{
		Name:        c.name,
		Version:     info.GetVersion(),
		Author:      info.GetAuthor(),
		Description: info.GetDescription(),
		Platforms:   append([]string(nil), platforms...),
		Permissions: intersectPermissions(declared, c.granted),
		Options:     append([]string(nil), info.GetOptions()...),
	}

	c.mu.Lock()
	c.meta = meta
	c.platform = platform
	c.mu.Unlock()
	return nil
}

// supervise 巡检连接状态：断连后按指数退避重新 Init 并重启已启动的实例；
// 连续失败达上限时停用实例并转入低速巡检，绝不退出核心进程。
func (c *Client) supervise(ctx context.Context) {
	backoff := reconnectInitialBackoff
	attempts := 0

	for {
		delay := reconnectPollInterval
		if attempts >= reconnectMaxAttempts {
			delay = reconnectDisabledPoll
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		switch state := c.conn.GetState(); state {
		case connectivity.Ready:
			attempts, backoff = 0, reconnectInitialBackoff
			continue
		case connectivity.Idle:
			// 空闲是 gRPC 的正常状态，主动触发一次连接即可，不算失败。
			c.conn.Connect()
			continue
		case connectivity.Connecting:
			continue
		}

		if err := c.reconnect(ctx); err != nil {
			attempts++
			if attempts >= reconnectMaxAttempts {
				c.setDisabled(true)
				c.log.Error("外部适配器重连失败达上限，停用其实例",
					"attempts", attempts, "error", err)
			} else {
				c.log.Warn("外部适配器重连失败", "attempt", attempts, "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, reconnectMaxBackoff)
			continue
		}
		attempts, backoff = 0, reconnectInitialBackoff
		c.setDisabled(false)
		c.log.Info("外部适配器已重连")
	}
}

// reconnect 重新 Init，并重启所有此前已启动的实例。
func (c *Client) reconnect(ctx context.Context) error {
	if err := c.init(ctx); err != nil {
		return err
	}
	for _, inst := range c.instanceSnapshot() {
		if !inst.startedFlag() {
			continue
		}
		inst.resetStarted()
		if err := inst.start(ctx); err != nil {
			return err
		}
	}
	return nil
}

// startSupervisor 只启动一次连接巡检，随 runCtx 结束。
func (c *Client) startSupervisor(ctx context.Context) {
	c.superviseOnce.Do(func() { go c.supervise(ctx) })
}

// instanceSnapshot 返回实例切片快照。
func (c *Client) instanceSnapshot() []*instance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*instance, 0, len(c.instances))
	for _, inst := range c.instances {
		out = append(out, inst)
	}
	return out
}

// setDisabled 批量设置实例的停用状态。
func (c *Client) setDisabled(disabled bool) {
	for _, inst := range c.instanceSnapshot() {
		inst.setDisabled(disabled)
	}
}

// rpcTimeout 返回单次 RPC 超时。
func (c *Client) rpcTimeout() time.Duration {
	if c.timeout <= 0 {
		return defaultTimeout
	}
	return c.timeout
}

// checkReservedOptions 校验保留键：配置了 listen_addr 就必须声明 net_listen。
func checkReservedOptions(botID string, settings map[string]any, adapter string, perms []bot.Permission) error {
	v, ok := settings[bot.OptListenAddr]
	if !ok || v == nil || strings.TrimSpace(fmt.Sprint(v)) == "" {
		return nil
	}
	if hasPermission(perms, bot.PermNetListen) {
		return nil
	}
	return fmt.Errorf("外部适配器 %s 的 bot %s 配置了保留键 %s，但未授予 %s 权限",
		adapter, botID, bot.OptListenAddr, bot.PermNetListen)
}

// resolvePlatform 依据配置与上报的平台列表确定平台名。
func resolvePlatform(adapter, explicit string, platforms []string) (string, error) {
	if explicit == "" {
		if len(platforms) != 1 {
			return "", fmt.Errorf("adaptermgr/external: 适配器 %s 上报 %d 个平台 %v，必须用 adapters.%s.platform 指定",
				adapter, len(platforms), platforms, adapter)
		}
		return platforms[0], nil
	}
	if !containsString(platforms, explicit) {
		return "", fmt.Errorf("adaptermgr/external: 适配器 %s 上报的平台 %v 不含配置中的 %q",
			adapter, platforms, explicit)
	}
	return explicit, nil
}

// target 把监听地址包装为 gRPC target 字符串（与外部插件加载器一致）。
func target(addr string) string {
	if strings.Contains(addr, "://") {
		return addr
	}
	return "passthrough:///" + addr
}

// encodeSettings 把配置编码为 JSON 字符串；nil 表示「没有配置」，编码为空串。
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

// encodeSettingsJSON 是 encodeSettings 的容错版本，用于重连路径。
func encodeSettingsJSON(settings map[string]any) string {
	out, _ := encodeSettings(settings)
	return out
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

// permissionsFromStrings 把 proto 的权限名转换为权限集合。
func permissionsFromStrings(names []string) []bot.Permission {
	out := make([]bot.Permission, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, bot.Permission(name))
		}
	}
	return out
}

// intersectPermissions 返回 declared ∩ granted，保持 declared 的顺序。
func intersectPermissions(declared, granted []bot.Permission) []bot.Permission {
	if len(granted) == 0 {
		return nil
	}
	out := make([]bot.Permission, 0, len(declared))
	for _, p := range declared {
		if hasPermission(granted, p) {
			out = append(out, p)
		}
	}
	return out
}

// platformsOrNil 返回长度<=1 的平台声明，用于 Init 请求。
func platformsOrNil(platform string) []string {
	if platform == "" {
		return nil
	}
	return []string{platform}
}

// containsString 判断字符串切片是否包含目标。
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// hasPermission 判断权限集合是否包含目标。
func hasPermission(perms []bot.Permission, p bot.Permission) bool {
	for _, x := range perms {
		if x == p {
			return true
		}
	}
	return false
}
