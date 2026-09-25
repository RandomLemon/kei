package myim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

const (
	// platformName 是写入 bot.Event.Platform 的平台标识，与注册元信息的 Platforms 一致。
	platformName = "myim"

	// sendPath 是平台发送接口路径，拼接在 api_base 之后。
	sendPath = "/message/send"

	// queueSize 是事件上行队列容量；回调线程只入队，投递由独立 goroutine 完成。
	queueSize = 256

	// maxBodyBytes 限制单次回调请求体与平台响应体的读取上限。
	maxBodyBytes = 1 << 20

	// emitTimeout 限制单次事件投递耗时。
	emitTimeout = 5 * time.Second

	// readHeaderTimeout 防止回调连接长时间不发送请求头。
	readHeaderTimeout = 5 * time.Second
)

// Options 是适配器实例的构造参数。
//
// 除 BotID/HTTPClient/Logger 来自 bot.AdapterContext 外，其余字段对应
// 实例私有配置键：api_base、listen_addr、path、self_id。
type Options struct {
	// BotID 是实例名（bot.AdapterContext.BotID），写入 bot.Event.BotID，必填。
	BotID string
	// APIBase 是平台开放接口地址，例如 https://im.example.com；为空时 Send 报错。
	APIBase string
	// ListenAddr 是入站回调监听地址，例如 127.0.0.1:19081，必填。
	ListenAddr string
	// Path 是入站回调路径，为空时使用 "/"；非空时必须以 "/" 开头。
	Path string
	// SelfID 是平台侧机器人账号，仅用于生成兜底事件 ID，为空时以 BotID 代替。
	SelfID string
	// HTTPClient 是出站请求客户端；核心只在声明 PermNetwork 时才注入，为 nil
	// 时不能发送消息（Send 返回错误）。
	HTTPClient *http.Client
	// Logger 可为 nil，为 nil 时使用 slog.Default()。
	Logger *slog.Logger
}

// Adapter 是 MyIM 平台适配器的一个实例，实现 bot.Adapter。
//
// 每个 bot 配置对应一个实例：可变的监听、队列与运行状态都挂在本结构体上，
// 同一平台可以并存多个互不影响的实例。
type Adapter struct {
	opts Options
	log  *slog.Logger
	caps bot.Capabilities

	mu      sync.Mutex
	sink    bot.EventSink
	srv     *http.Server
	ln      net.Listener
	addr    string
	queue   chan *bot.Event
	stop    chan struct{}
	done    chan struct{}
	stopped bool

	stopOnce sync.Once
	seq      atomic.Uint64
}

// 确保 Adapter 满足 bot.Adapter。
var _ bot.Adapter = (*Adapter)(nil)

// NewFromOptions 依据显式参数构造适配器，供嵌入式使用与测试。
//
// 与核心工厂的区别：这里不校验 HTTPClient，允许先构造实例再观察 Send 的报错。
func NewFromOptions(opts Options) (*Adapter, error) {
	if opts.BotID == "" {
		return nil, errors.New("myim: BotID 不能为空")
	}
	if opts.ListenAddr == "" {
		return nil, errors.New("myim: ListenAddr 不能为空")
	}
	if opts.Path != "" && !strings.HasPrefix(opts.Path, "/") {
		return nil, fmt.Errorf("myim: Path 必须以 / 开头，当前为 %q", opts.Path)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Adapter{
		opts: opts,
		log:  log.With("bot", opts.BotID, "adapter", platformName),
		caps: bot.Capabilities{
			Text:    true,
			At:      true,
			Reply:   true,
			Private: true,
			Group:   true,
		},
	}, nil
}

// Name 返回平台名。
func (a *Adapter) Name() string { return platformName }

// Capabilities 返回平台能力，构造时就已固定，不随后续调用变化。
func (a *Adapter) Capabilities() bot.Capabilities { return a.caps }

// Addr 返回入站回调实际监听地址，未启动时为 ""。
func (a *Adapter) Addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addr
}

// callbackPath 返回入站回调路径，未配置时为 "/"。
func (a *Adapter) callbackPath() string {
	if a.opts.Path == "" {
		return "/"
	}
	return a.opts.Path
}

// selfID 返回用于生成兜底事件 ID 的机器人账号。
func (a *Adapter) selfID() string {
	if a.opts.SelfID != "" {
		return a.opts.SelfID
	}
	return a.opts.BotID
}

// Start 启动入站回调监听，随后阻塞到 ctx 结束。
//
// 返回前已完成 net.Listen，因此 Start 尚在阻塞时端口就已可接收请求；
// 平台回调先收到 202，再由独立的投递 goroutine 调用 sink.Emit，
// 回调线程不会因业务处理而阻塞。
func (a *Adapter) Start(ctx context.Context, sink bot.EventSink) error {
	if sink == nil {
		return errors.New("myim: EventSink 不能为空")
	}

	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return errors.New("myim: 适配器已停止，不能再次启动")
	}
	if a.sink != nil {
		a.mu.Unlock()
		return errors.New("myim: 适配器已在运行")
	}
	ln, err := net.Listen("tcp", a.opts.ListenAddr)
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("myim: 监听 %s 失败: %w", a.opts.ListenAddr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(a.callbackPath(), a.handleCallback)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
	queue := make(chan *bot.Event, queueSize)
	stop := make(chan struct{})
	done := make(chan struct{})

	a.sink = sink
	a.srv = srv
	a.ln = ln
	a.addr = ln.Addr().String()
	a.queue = queue
	a.stop = stop
	a.done = done
	a.mu.Unlock()

	go func() {
		defer close(done)
		a.dispatch(stop, sink, queue)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("myim: 入站回调服务退出", "error", err)
		}
	}()
	a.log.Info("myim: 入站回调已监听", "addr", ln.Addr().String(), "path", a.callbackPath())

	<-ctx.Done()
	return nil
}

// Stop 关闭入站回调服务并排空已接收的事件，可重复调用且都返回 nil。
func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	a.stopped = true
	srv, stop, done := a.srv, a.stop, a.done
	a.mu.Unlock()

	var err error
	a.stopOnce.Do(func() {
		if srv != nil {
			if shutErr := srv.Shutdown(ctx); shutErr != nil {
				err = fmt.Errorf("myim: 关闭入站回调服务失败: %w", shutErr)
			}
		}
		if stop != nil {
			close(stop)
		}
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
	})
	return err
}

// dispatch 串行消费事件队列；stop 关闭后先把队列排空再退出。
func (a *Adapter) dispatch(stop <-chan struct{}, sink bot.EventSink, queue <-chan *bot.Event) {
	for {
		select {
		case <-stop:
			for {
				select {
				case ev := <-queue:
					a.emit(sink, ev)
				default:
					return
				}
			}
		case ev := <-queue:
			a.emit(sink, ev)
		}
	}
}

// emit 投递单个事件；失败只记 warn 日志，不阻塞后续事件。
func (a *Adapter) emit(sink bot.EventSink, ev *bot.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), emitTimeout)
	defer cancel()
	if err := sink.Emit(ctx, ev); err != nil {
		a.log.Warn("myim: 投递事件失败", "event_id", ev.ID, "error", err)
	}
}

// callbackEvent 是 MyIM 平台推送的事件回调体。
type callbackEvent struct {
	// ID 是平台事件 ID，缺失时由适配器生成兜底 ID。
	ID string `json:"id"`
	// Type 是平台事件类型：message/notice/request/meta，其它值按 message 处理。
	Type string `json:"type"`
	// Time 是事件发生时间（Unix 秒），为 0 时取适配器收到的当前时间。
	Time int64 `json:"time"`
	// MessageID 是平台消息 ID。
	MessageID string `json:"message_id"`
	// UserID 与 UserName 描述发送者。
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	// GroupID 与 GroupName 描述群会话，为空表示私聊。
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name"`
	// Text 是消息纯文本内容。
	Text string `json:"text"`
}

// handleCallback 处理平台回调：先 202 应答，再把事件异步投递到核心。
func (a *Adapter) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "只接受 POST 回调", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "读取回调请求体失败", http.StatusBadRequest)
		return
	}
	var cb callbackEvent
	if err := json.Unmarshal(body, &cb); err != nil {
		http.Error(w, "解析平台事件失败", http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	queue, stopped := a.queue, a.stopped
	a.mu.Unlock()
	if stopped || queue == nil {
		http.Error(w, "适配器未运行", http.StatusServiceUnavailable)
		return
	}

	ev := a.buildEvent(&cb, body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"code":0}`))

	select {
	case queue <- ev:
	default:
		a.log.Warn("myim: 事件队列已满，丢弃回调事件", "event_id", ev.ID)
	}
}

// buildEvent 把平台回调转换为统一事件。
func (a *Adapter) buildEvent(cb *callbackEvent, raw []byte) *bot.Event {
	ev := &bot.Event{
		ID:       firstNonEmpty(cb.ID, fmt.Sprintf("myim-%s-%d", a.selfID(), a.seq.Add(1))),
		Type:     eventType(cb.Type),
		Platform: platformName,
		BotID:    a.opts.BotID,
		Time:     eventTime(cb.Time),
		Raw:      json.RawMessage(raw),
	}
	kind := bot.MessagePrivate
	if cb.GroupID != "" {
		kind = bot.MessageGroup
	}
	if ev.Type == bot.EventMessage {
		segs := make([]bot.Segment, 0, 1)
		if cb.Text != "" {
			segs = append(segs, bot.Segment{Type: bot.SegText, Data: map[string]any{bot.KeyText: cb.Text}})
		}
		ev.Message = &bot.Message{ID: cb.MessageID, Kind: kind, Segments: segs}
	}
	if cb.UserID != "" {
		ev.Sender = &bot.User{ID: cb.UserID, Name: cb.UserName}
	}
	if cb.GroupID != "" {
		ev.Channel = &bot.Channel{ID: cb.GroupID, Name: cb.GroupName, Kind: bot.MessageGroup}
	}
	return ev
}

// eventType 把平台事件类型映射为统一事件类型，未知类型按消息处理。
func eventType(t string) bot.EventType {
	switch t {
	case string(bot.EventNotice):
		return bot.EventNotice
	case string(bot.EventRequest):
		return bot.EventRequest
	case string(bot.EventMeta):
		return bot.EventMeta
	default:
		return bot.EventMessage
	}
}

// eventTime 把回调里的 Unix 秒转换为 UTC 时间，缺失时取当前时间。
func eventTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Now().UTC()
	}
	return time.Unix(sec, 0).UTC()
}

// platformTarget 是发送请求里的接收方。
type platformTarget struct {
	Kind      string `json:"kind,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
}

// platformSegment 是发送请求里的消息段。
type platformSegment struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

// platformMessage 是发送请求里的消息体。
type platformMessage struct {
	Kind     string            `json:"kind,omitempty"`
	Segments []platformSegment `json:"segments"`
}

// sendPayload 是发往平台发送接口的请求体。
type sendPayload struct {
	Target  platformTarget  `json:"target"`
	Message platformMessage `json:"message"`
	ReplyTo string          `json:"reply_to,omitempty"`
}

// sendResponse 是平台发送接口的响应体。
type sendResponse struct {
	Code      int    `json:"code"`
	MessageID string `json:"message_id"`
}

// Send 把统一消息降级后发往平台开放接口。
//
// 发送前按本平台能力降级（Capabilities 未声明的段转文本，见 bot.Degrade）；
// 降级后没有可发送的段时返回错误，不静默丢消息。
func (a *Adapter) Send(ctx context.Context, req *bot.SendRequest) (*bot.SendResult, error) {
	if req == nil {
		return nil, errors.New("myim: 发送请求不能为空")
	}
	if a.opts.APIBase == "" {
		return nil, fmt.Errorf("myim: bot %q 未配置 api_base，无法发送消息", a.opts.BotID)
	}
	if a.opts.HTTPClient == nil {
		return nil, fmt.Errorf("myim: bot %q 未获得 HTTPClient（未声明 PermNetwork），无法发送消息", a.opts.BotID)
	}
	msg := bot.Degrade(req.Message, a.caps)
	if msg == nil || len(msg.Segments) == 0 {
		return nil, fmt.Errorf("myim: 消息在按平台能力降级后没有可发送的段（bot %s）", a.opts.BotID)
	}

	payload := sendPayload{
		Target: platformTarget{
			Kind:      string(req.Target.Kind),
			UserID:    req.Target.UserID,
			ChannelID: req.Target.ChannelID,
		},
		Message: platformMessageFrom(msg),
		ReplyTo: req.ReplyTo,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("myim: 编码发送请求失败: %w", err)
	}

	url := strings.TrimRight(a.opts.APIBase, "/") + sendPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("myim: 构造发送请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := a.opts.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("myim: 调用 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("myim: 读取平台响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("myim: 发送失败，平台返回 %s: %s", resp.Status, truncate(data))
	}
	var out sendResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("myim: 解析平台响应失败: %w", err)
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("myim: 发送失败，平台返回 code=%d", out.Code)
	}
	if out.MessageID == "" {
		return nil, fmt.Errorf("myim: 平台响应缺少 message_id（bot %s）", a.opts.BotID)
	}
	return &bot.SendResult{MessageID: out.MessageID, Raw: out}, nil
}

// platformMessageFrom 把统一消息转换为平台消息体。
func platformMessageFrom(msg *bot.Message) platformMessage {
	out := platformMessage{Kind: string(msg.Kind), Segments: make([]platformSegment, 0, len(msg.Segments))}
	for _, seg := range msg.Segments {
		switch seg.Type {
		case bot.SegText:
			out.Segments = append(out.Segments, platformSegment{
				Type: string(bot.SegText),
				Text: anyString(seg.Data[bot.KeyText]),
			})
		case bot.SegAt:
			out.Segments = append(out.Segments, platformSegment{
				Type:   string(bot.SegAt),
				UserID: anyString(seg.Data[bot.KeyUserID]),
			})
		case bot.SegReply:
			out.Segments = append(out.Segments, platformSegment{
				Type:      string(bot.SegReply),
				MessageID: anyString(seg.Data[bot.KeyMessageID]),
			})
		default:
			// Send 前已按能力降级，这里只是兜底：未知段按文本转发而非丢弃。
			out.Segments = append(out.Segments, platformSegment{
				Type: string(bot.SegText),
				Text: "[" + string(seg.Type) + "]",
			})
		}
	}
	return out
}

// anyString 把消息段的取值转换为字符串，nil 得到空串。
func anyString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// truncate 截断响应体，避免把大段内容写进错误信息。
func truncate(data []byte) string {
	const max = 256
	if len(data) <= max {
		return string(data)
	}
	return string(data[:max]) + "..."
}
