package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/grpc"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

// extPrefix 是示例插件识别的命令前缀。
const extPrefix = "/ext"

// keyword 是示例插件识别的关键词。
const keyword = "external"

// options 是示例插件的构造参数。
//
// 字段与命令行 flag 一一对应，作为独立的 options 结构而非直接读 flag，
// 是为了让插件逻辑可在单元测试与集成测试中直接构造。
type options struct {
	// name 是插件名，与核心侧配置的插件键一致。
	name string
	// greeting 是回复前缀，最终消息形如 "<greeting>: <内容>"。
	greeting string
	// token 是核心分配的令牌，用于校验 Init 与反向调用核心。
	token string
	// coreAddr 是核心 BotService 地址，仅用于日志与排查。
	coreAddr string
	// coreConn 是连接核心 BotService 的客户端连接，必填。
	coreConn *grpc.ClientConn
	// logger 是日志器，nil 时使用 slog.Default()。
	logger *slog.Logger
}

// plugin 是示例外部插件的 PluginService 实现。
//
// 它演示外部插件的完整往返：核心通过 HandleEvent 投递事件，插件判定后经
// BotService.SendMessage 反向调用核心发送回复。plugin 并发安全。
type plugin struct {
	pluginpb.UnimplementedPluginServiceServer

	name     string
	greeting string
	token    string
	coreAddr string
	core     pluginpb.BotServiceClient
	logger   *slog.Logger

	// mu 保护 Init 写入、HandleEvent 读取的运行时状态。
	mu          sync.RWMutex
	settings    map[string]any
	permissions []string
}

// newPlugin 构造示例插件。
//
// name、token、coreConn 缺失时返回错误：这些是插件与核心通信的前提，
// 必须在进程启动阶段暴露而不是等到第一条事件。
func newPlugin(opts options) (*plugin, error) {
	if opts.name == "" {
		return nil, fmt.Errorf("example-plugin: name 不能为空")
	}
	if opts.token == "" {
		return nil, fmt.Errorf("example-plugin: token 不能为空")
	}
	if opts.coreConn == nil {
		return nil, fmt.Errorf("example-plugin: coreConn 不能为空")
	}
	logger := opts.logger
	if logger == nil {
		logger = slog.Default()
	}
	greeting := opts.greeting
	if greeting == "" {
		return nil, fmt.Errorf("example-plugin: greeting 不能为空")
	}
	return &plugin{
		name:     opts.name,
		greeting: greeting,
		token:    opts.token,
		coreAddr: opts.coreAddr,
		core:     pluginpb.NewBotServiceClient(opts.coreConn),
		logger:   logger.With("plugin", opts.name),
	}, nil
}

// Init 校验令牌并保存核心下发的配置与权限。
//
// 令牌不匹配时返回 ok=false 而不是 RPC 错误：这是业务拒绝，核心据此知道
// 「插件进程活着但拒绝服务」，与传输失败区分开。配置解析失败同样返回 ok=false，
// 避免插件带着残缺的配置继续运行。
func (p *plugin) Init(_ context.Context, req *pluginpb.InitRequest) (*pluginpb.InitResponse, error) {
	if req.GetToken() != p.token {
		p.logger.Warn("初始化被拒绝", "reason", "token 不匹配")
		return &pluginpb.InitResponse{Ok: false, Error: "bad token"}, nil
	}

	settings := make(map[string]any)
	if raw := req.GetConfigJson(); raw != "" {
		if err := json.Unmarshal([]byte(raw), &settings); err != nil {
			p.logger.Warn("初始化被拒绝", "reason", "config_json 非法", "err", err)
			return &pluginpb.InitResponse{Ok: false, Error: "bad config: " + err.Error()}, nil
		}
	}

	p.mu.Lock()
	p.settings = settings
	p.permissions = append([]string(nil), req.GetPermissions()...)
	p.mu.Unlock()

	p.logger.Info("外部插件已初始化",
		"core_addr", req.GetReplyServiceAddr(),
		"permissions", req.GetPermissions(),
		"config_keys", len(settings),
	)
	return &pluginpb.InitResponse{Ok: true}, nil
}

// HandleEvent 处理核心投递的事件。
//
// 只处理纯文本消息事件，且仅在文本命中 /ext 命令或 external 关键词时回复；
// 其余情况返回 handled=false 且不调用核心，把事件交还给核心侧的其它插件。
//
// 失败语义：发送失败（传输错误或核心返回的业务错误）写入 HandleResult.Error
// 并保持 handled=true —— 事件确实被本插件消费了，只是回复没发出去。
func (p *plugin) HandleEvent(ctx context.Context, ev *pluginpb.Event) (*pluginpb.HandleResult, error) {
	if ev.GetType() != string(bot.EventMessage) {
		return &pluginpb.HandleResult{}, nil
	}
	text, ok := plainText(ev.GetMessage())
	if !ok {
		return &pluginpb.HandleResult{}, nil
	}

	content, ok := p.replyContent(text)
	if !ok {
		return &pluginpb.HandleResult{}, nil
	}

	target := targetOf(ev)
	resp, err := p.core.SendMessage(ctx, &pluginpb.SendRequest{
		Token:   p.token,
		Target:  target,
		Message: textMessage(target.GetKind(), p.greeting+": "+content),
	})
	if err != nil {
		return &pluginpb.HandleResult{Handled: true, Error: err.Error()}, nil
	}
	if msg := resp.GetError(); msg != "" {
		return &pluginpb.HandleResult{Handled: true, Error: msg}, nil
	}
	return &pluginpb.HandleResult{Handled: true}, nil
}

// Shutdown 返回空应答；示例插件没有需要释放的后台资源。
func (p *plugin) Shutdown(context.Context, *pluginpb.Empty) (*pluginpb.Empty, error) {
	p.logger.Info("外部插件收到退出通知")
	return &pluginpb.Empty{}, nil
}

// replyContent 判定事件文本是否触发回复，并返回应回复的内容。
//
// 规则：以 /ext 开头时取其后的内容；否则包含 external 时回复原文。
// 命中的内容为空（例如仅有 "/ext"）时视为未触发。
func (p *plugin) replyContent(text string) (string, bool) {
	if strings.HasPrefix(text, extPrefix) {
		if content := strings.TrimSpace(text[len(extPrefix):]); content != "" {
			return content, true
		}
		return "", false
	}
	if strings.Contains(text, keyword) {
		return text, true
	}
	return "", false
}

// plainText 提取消息的纯文本内容。
//
// 只有全部由文本段（text/markdown）组成且拼接结果非空的消息才被接受；
// 含图片、@、卡片等混合内容的消息直接放行给其它插件。
func plainText(msg *pluginpb.Message) (string, bool) {
	segs := msg.GetSegments()
	if len(segs) == 0 {
		return "", false
	}
	var b strings.Builder
	for _, seg := range segs {
		if bot.SegmentType(seg.GetType()) != bot.SegText &&
			bot.SegmentType(seg.GetType()) != bot.SegMarkdown {
			return "", false
		}
		var data struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(seg.GetDataJson()), &data); err != nil {
			return "", false
		}
		b.WriteString(data.Text)
	}
	text := b.String()
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// targetOf 从事件回填发送目标。
//
// 会话类型优先取消息自身的 Kind，缺失时退回会话对象的 Kind；reply_to 留空
// 表示直接发送而非引用回复。
func targetOf(ev *pluginpb.Event) *pluginpb.Target {
	kind := ev.GetMessage().GetKind()
	if kind == "" {
		kind = ev.GetChannel().GetKind()
	}
	return &pluginpb.Target{
		Platform:  ev.GetPlatform(),
		ChannelId: ev.GetChannel().GetId(),
		UserId:    ev.GetSender().GetId(),
		Kind:      kind,
	}
}

// textMessage 构造只含一个文本段的回复消息。
//
// kind 取自发送目标，保证回复与会话类型一致（私聊回复不会变成群消息）。
func textMessage(kind, text string) *pluginpb.Message {
	data, err := json.Marshal(map[string]string{bot.KeyText: text})
	if err != nil {
		// map[string]string 的编码不会失败，保留兜底以避免返回空段。
		data = []byte(`{"text":""}`)
	}
	return &pluginpb.Message{
		Kind:     string(bot.MessageGroup),
		Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: string(data)}},
	}
}
