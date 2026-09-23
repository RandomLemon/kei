package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// 发送相关常量。
const (
	// tokenPath 是获取 tenant_access_token 的接口路径。
	tokenPath = "/open-apis/auth/v3/tenant_access_token/internal"
	// messagesPath 是发送消息的接口路径。
	messagesPath = "/open-apis/im/v1/messages"
	// imagesPath 是上传图片的接口路径。
	imagesPath = "/open-apis/im/v1/images"
	// tokenRefreshMargin 是 token 提前刷新余量。
	tokenRefreshMargin = 5 * time.Minute
	// maxImageBytes 是允许下载的图片大小上限。
	maxImageBytes = 5 << 20
	// maxResponseBytes 是读取平台响应的大小上限。
	maxResponseBytes = 1 << 20

	// msgTypeText 是文本消息类型。
	msgTypeText = "text"
	// msgTypeImage 是图片消息类型。
	msgTypeImage = "image"
	// msgTypeFile 是文件消息类型。
	msgTypeFile = "file"
	// msgTypeInteractive 是卡片消息类型。
	msgTypeInteractive = "interactive"

	// receiveIDTypeOpenID 表示 receive_id 使用用户 open_id。
	receiveIDTypeOpenID = "open_id"
	// receiveIDTypeChatID 表示 receive_id 使用会话 chat_id。
	receiveIDTypeChatID = "chat_id"
)

// authResponse 是 tenant_access_token 接口响应。
type authResponse struct {
	// Code 为 0 表示成功。
	Code int `json:"code"`
	// Msg 是失败描述。
	Msg string `json:"msg"`
	// TenantAccessToken 是租户访问令牌。
	TenantAccessToken string `json:"tenant_access_token"`
	// Expire 是令牌有效期，单位秒。
	Expire int `json:"expire"`
}

// sendMessageResponse 是发送消息接口响应。
type sendMessageResponse struct {
	// Code 为 0 表示成功。
	Code int `json:"code"`
	// Msg 是失败描述。
	Msg string `json:"msg"`
	// Data 是成功时的业务数据。
	Data struct {
		// MessageID 是发送成功后的消息 ID。
		MessageID string `json:"message_id"`
	} `json:"data"`
}

// uploadImageResponse 是上传图片接口响应。
type uploadImageResponse struct {
	// Code 为 0 表示成功。
	Code int `json:"code"`
	// Msg 是失败描述。
	Msg string `json:"msg"`
	// Data 是成功时的业务数据。
	Data struct {
		// ImageKey 是上传得到的图片标识。
		ImageKey string `json:"image_key"`
	} `json:"data"`
}

// chunk 是与飞书 API 一次交互对应的消息片段。
//
// 飞书一条消息只能有一种 msg_type，因此统一消息会被切成若干块，
// 每块单独发送：连续文本/@ 合并为一条 text，每张图片单独一条 image，
// 卡片单独一条 interactive。
type chunk struct {
	// msgType 是飞书消息类型。
	msgType string
	// content 是飞书要求的 content JSON 字符串。
	content string
}

// Send 把统一消息转换为飞书消息并发送。
//
// 发送前按平台能力降级；消息含多段且类型不同时会拆成多条消息依次发送，
// 返回最后一条消息的 ID。任一请求失败即返回错误。
func (a *Adapter) Send(ctx context.Context, req *bot.SendRequest) (*bot.SendResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil || req.Message == nil {
		return nil, errors.New("feishu: SendRequest 与 Message 不能为空")
	}
	chunks, err := a.buildChunks(ctx, bot.Degrade(req.Message, a.Capabilities()))
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, errors.New("feishu: 消息没有可发送的内容")
	}

	token, err := a.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	replyTo := req.ReplyTo
	if replyTo == "" {
		replyTo = replyTarget(req.Message)
	}
	// 回复路径不需要 receive_id；直发路径必须能解析出目标。
	var receiveID, receiveIDType string
	if replyTo == "" {
		var err error
		if receiveID, receiveIDType, err = receiveTarget(req.Target); err != nil {
			return nil, err
		}
	}

	var (
		lastID  string
		lastRaw json.RawMessage
	)
	for i := range chunks {
		raw, id, err := a.postChunk(ctx, token, replyTo, receiveID, receiveIDType, chunks[i])
		if err != nil {
			return nil, err
		}
		lastRaw, lastID = raw, id
	}
	return &bot.SendResult{MessageID: lastID, Raw: lastRaw}, nil
}

// replyTarget 从消息中取出引用段指定的消息 ID。
func replyTarget(msg *bot.Message) string {
	for i := range msg.Segments {
		if msg.Segments[i].Type == bot.SegReply {
			return dataString(msg.Segments[i], bot.KeyMessageID)
		}
	}
	return ""
}

// receiveTarget 计算发送目标与 receive_id_type：
// 私聊使用 open_id（取 UserID），其余使用 chat_id（取 ChannelID）。
func receiveTarget(t bot.Target) (receiveID, receiveIDType string, err error) {
	if t.Kind == bot.MessagePrivate {
		if t.UserID == "" {
			return "", "", errors.New("feishu: 私聊发送需要 Target.UserID")
		}
		return t.UserID, receiveIDTypeOpenID, nil
	}
	if t.ChannelID == "" {
		return "", "", errors.New("feishu: 群聊发送需要 Target.ChannelID")
	}
	return t.ChannelID, receiveIDTypeChatID, nil
}

// buildChunks 把统一消息段切分为可发送的分块。
//
// 图片段若给出 KeyURL（http/https）会先下载并上传到飞书换取 image_key；
// 给出 KeyFile 时直接当作 image_key 使用。文件段仅在给出 KeyFile 时按
// file 类型发送，只有 URL 时降级为带链接的文本。
func (a *Adapter) buildChunks(ctx context.Context, msg *bot.Message) ([]chunk, error) {
	var (
		chunks  []chunk
		textBuf strings.Builder
	)
	flushText := func() {
		if textBuf.Len() == 0 {
			return
		}
		if c, err := textChunk(textBuf.String()); err == nil {
			chunks = append(chunks, c)
		}
		textBuf.Reset()
	}

	for i := range msg.Segments {
		seg := msg.Segments[i]
		switch seg.Type {
		case bot.SegText, bot.SegMarkdown:
			// Markdown 段按纯文本降级发送；飞书 text 本身不渲染 Markdown，
			// 需要富文本时应改用卡片段。
			textBuf.WriteString(dataString(seg, bot.KeyText))

		case bot.SegAt:
			id := dataString(seg, bot.KeyUserID)
			if id == "" {
				a.log.Warn("飞书消息 @ 段缺少 user_id，已跳过", "name", dataString(seg, bot.KeyUserName))
				continue
			}
			textBuf.WriteString(`<at user_id="`)
			textBuf.WriteString(id)
			textBuf.WriteString(`"></at>`)

		case bot.SegImage:
			flushText()
			key, err := a.resolveImageKey(ctx, seg)
			if err != nil {
				return nil, err
			}
			c, err := jsonChunk(msgTypeImage, map[string]string{"image_key": key})
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, c)

		case bot.SegCard:
			flushText()
			content, err := cardContent(seg)
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, chunk{msgType: msgTypeInteractive, content: content})

		case bot.SegFile:
			flushText()
			if key := dataString(seg, bot.KeyFile); key != "" {
				c, err := jsonChunk(msgTypeFile, map[string]string{"file_key": key})
				if err != nil {
					return nil, err
				}
				chunks = append(chunks, c)
				continue
			}
			textBuf.WriteString("[file] ")
			textBuf.WriteString(dataString(seg, bot.KeyFileName))
			textBuf.WriteString(" ")
			textBuf.WriteString(dataString(seg, bot.KeyURL))

		case bot.SegReply:
			// 引用信息已由 replyTarget 处理，不参与消息内容。

		case bot.SegFace:
			textBuf.WriteString("[" + dataString(seg, bot.KeyFaceID) + "]")

		default:
			a.log.Warn("飞书适配器忽略未知消息段", "segment_type", string(seg.Type))
		}
	}
	flushText()
	return chunks, nil
}

// textChunk 构造 text 类型分块。
func textChunk(text string) (chunk, error) {
	b, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return chunk{}, fmt.Errorf("feishu: 序列化文本消息失败: %w", err)
	}
	return chunk{msgType: msgTypeText, content: string(b)}, nil
}

// jsonChunk 构造以 JSON 对象为 content 的分块。
func jsonChunk(msgType string, payload any) (chunk, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return chunk{}, fmt.Errorf("feishu: 序列化 %s 消息失败: %w", msgType, err)
	}
	return chunk{msgType: msgType, content: string(b)}, nil
}

// cardContent 把卡片段转为飞书 interactive 消息的 content。
func cardContent(seg bot.Segment) (string, error) {
	switch v := seg.Data[bot.KeyCard].(type) {
	case nil:
		return "", errors.New("feishu: 卡片段缺少内容")
	case string:
		if v == "" {
			return "", errors.New("feishu: 卡片段内容为空")
		}
		return v, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("feishu: 序列化卡片失败: %w", err)
		}
		return string(b), nil
	}
}

// resolveImageKey 得到可发送的 image_key：KeyFile 直接使用，KeyURL 先下载再上传。
func (a *Adapter) resolveImageKey(ctx context.Context, seg bot.Segment) (string, error) {
	if key := dataString(seg, bot.KeyFile); key != "" {
		return key, nil
	}
	raw := dataString(seg, bot.KeyURL)
	if !isHTTPURL(raw) {
		return "", fmt.Errorf("feishu: 图片段既无有效 file 也无 http(s) url: %q", raw)
	}
	data, name, err := a.downloadImage(ctx, raw)
	if err != nil {
		return "", err
	}
	return a.uploadImage(ctx, data, name)
}

// isHTTPURL 判断字符串是否为 http/https 地址。
func isHTTPURL(raw string) bool {
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}

// downloadImage 下载图片，超过 5MB 时返回错误。
func (a *Adapter) downloadImage(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("feishu: 构造图片下载请求失败: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("feishu: 下载图片失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("feishu: 下载图片返回状态 %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("feishu: 读取图片内容失败: %w", err)
	}
	if len(data) > maxImageBytes {
		return nil, "", fmt.Errorf("feishu: 图片超过 %d 字节上限", maxImageBytes)
	}
	name := path.Base(req.URL.Path)
	if name == "" || name == "." || name == "/" {
		name = "image"
	}
	return data, name, nil
}

// uploadImage 上传图片到飞书并返回 image_key。
func (a *Adapter) uploadImage(ctx context.Context, data []byte, name string) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("image_type", "message"); err != nil {
		return "", fmt.Errorf("feishu: 写入 image_type 失败: %w", err)
	}
	part, err := w.CreateFormFile("image", name)
	if err != nil {
		return "", fmt.Errorf("feishu: 创建图片表单字段失败: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("feishu: 写入图片内容失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("feishu: 结束图片表单失败: %w", err)
	}

	token, err := a.accessToken(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.opts.BaseURL+imagesPath, &body)
	if err != nil {
		return "", fmt.Errorf("feishu: 构造图片上传请求失败: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	raw, err := a.do(req)
	if err != nil {
		return "", err
	}
	var out uploadImageResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("feishu: 解析图片上传响应失败: %w", err)
	}
	if out.Code != 0 {
		return "", fmt.Errorf("feishu: 上传图片失败 code=%d msg=%s", out.Code, out.Msg)
	}
	if out.Data.ImageKey == "" {
		return "", errors.New("feishu: 图片上传成功但未返回 image_key")
	}
	return out.Data.ImageKey, nil
}

// postChunk 发送一个分块，返回原始响应与消息 ID。
func (a *Adapter) postChunk(ctx context.Context, token, replyTo, receiveID, receiveIDType string, c chunk) (json.RawMessage, string, error) {
	var (
		endpoint string
		payload  map[string]string
	)
	if replyTo != "" {
		endpoint = a.opts.BaseURL + messagesPath + "/" + url.PathEscape(replyTo) + "/reply"
		payload = map[string]string{"msg_type": c.msgType, "content": c.content}
	} else {
		endpoint = a.opts.BaseURL + messagesPath + "?receive_id_type=" + url.QueryEscape(receiveIDType)
		payload = map[string]string{"receive_id": receiveID, "msg_type": c.msgType, "content": c.content}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("feishu: 序列化发送请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("feishu: 构造发送请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)

	raw, err := a.do(req)
	if err != nil {
		return nil, "", err
	}
	var out sendMessageResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "", fmt.Errorf("feishu: 解析发送响应失败: %w", err)
	}
	if out.Code != 0 {
		return nil, "", fmt.Errorf("feishu: 发送消息失败 code=%d msg=%s", out.Code, out.Msg)
	}
	return raw, out.Data.MessageID, nil
}

// accessToken 返回可用的 tenant_access_token，必要时刷新。
//
// 缓存命中时直接返回；未命中时用 refreshMu 串行化刷新，保证并发请求
// 不会重复调用令牌接口。返回的令牌至少还有 5 分钟有效期
// （令牌有效期本身不足 10 分钟时提前一半刷新）。
func (a *Adapter) accessToken(ctx context.Context) (string, error) {
	if tok, ok := a.cachedToken(); ok {
		return tok, nil
	}
	a.tokRefresh.Lock()
	defer a.tokRefresh.Unlock()
	if tok, ok := a.cachedToken(); ok {
		return tok, nil
	}
	tok, expire, err := a.fetchToken(ctx)
	if err != nil {
		return "", err
	}
	margin := tokenRefreshMargin
	if expire <= 2*margin {
		margin = expire / 2
	}
	a.tokMu.Lock()
	a.tok, a.tokExpire = tok, time.Now().UTC().Add(expire-margin)
	a.tokMu.Unlock()
	return tok, nil
}

// cachedToken 返回仍在有效期内的缓存令牌。
func (a *Adapter) cachedToken() (string, bool) {
	a.tokMu.RLock()
	defer a.tokMu.RUnlock()
	if a.tok == "" || !time.Now().UTC().Before(a.tokExpire) {
		return "", false
	}
	return a.tok, true
}

// fetchToken 调用飞书接口获取 tenant_access_token。
func (a *Adapter) fetchToken(ctx context.Context) (string, time.Duration, error) {
	body, err := json.Marshal(map[string]string{"app_id": a.opts.AppID, "app_secret": a.opts.AppSecret})
	if err != nil {
		return "", 0, fmt.Errorf("feishu: 序列化令牌请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.opts.BaseURL+tokenPath, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("feishu: 构造令牌请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	raw, err := a.do(req)
	if err != nil {
		return "", 0, err
	}
	var out authResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, fmt.Errorf("feishu: 解析令牌响应失败: %w", err)
	}
	if out.Code != 0 {
		return "", 0, fmt.Errorf("feishu: 获取 tenant_access_token 失败 code=%d msg=%s", out.Code, out.Msg)
	}
	if out.TenantAccessToken == "" {
		return "", 0, errors.New("feishu: 令牌响应缺少 tenant_access_token")
	}
	expire := time.Duration(out.Expire) * time.Second
	if expire <= 0 {
		expire = tokenRefreshMargin
	}
	return out.TenantAccessToken, expire, nil
}

// do 执行请求并读取响应体，非 2xx 状态码也返回错误。
func (a *Adapter) do(req *http.Request) ([]byte, error) {
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("feishu: 请求 %s 失败: %w", req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("feishu: 读取 %s 响应失败: %w", req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("feishu: 请求 %s 返回状态 %d: %s", req.URL.Path, resp.StatusCode, truncate(raw))
	}
	return raw, nil
}

// truncate 截断错误信息中的响应体，避免日志过长。
func truncate(b []byte) string {
	const max = 256
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// dataString 读取消息段中的字符串字段。
func dataString(seg bot.Segment, key string) string {
	if seg.Data == nil {
		return ""
	}
	if s, ok := seg.Data[key].(string); ok {
		return s
	}
	return ""
}
