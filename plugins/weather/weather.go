// Package weather 演示外部 HTTP 调用与多轮对话的示例插件。
//
// 触发方式：
//   - /weather 北京      —— 命令触发，参数为城市名
//   - 天气 北京          —— 正则触发，捕获城市名
//   - /weather           —— 进入多轮对话，下一条非命令消息被当作城市名
//
// 默认使用免密钥的 open-meteo 接口，可用插件配置覆盖：
//
//	geocode_url / forecast_url / api_key / api_key_header / api_key_prefix / timeout
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

const (
	defaultGeocodeURL  = "https://geocoding-api.open-meteo.com/v1/search"
	defaultForecastURL = "https://api.open-meteo.com/v1/forecast"
	defaultTimeout     = 8 * time.Second
	pendingTTL         = 2 * time.Minute
	maxBodyBytes       = 1 << 20
	pendingKeyPrefix   = "weather:pending:"
)

// Plugin 是天气插件。
type Plugin struct {
	log   *slog.Logger
	http  *http.Client
	store bot.Storage
	cfg   *bot.Config

	geocodeURL  string
	forecastURL string
	apiKey      string
	apiHeader   string
	apiPrefix   string
	timeout     time.Duration
}

// 确保 Plugin 满足 bot.Plugin。
var _ bot.Plugin = (*Plugin)(nil)

// Metadata 返回插件元信息。
func (p *Plugin) Metadata() bot.Metadata {
	return bot.Metadata{
		Name:        "weather",
		Version:     "v0.1.0",
		Author:      "core",
		Description: "查询城市天气：/weather <城市> 或 天气 <城市>",
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermStorage},
	}
}

// Setup 读取插件配置并注册触发规则。
func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	pc, ok := bot.PluginContextFrom(ctx)
	if !ok {
		return errors.New("weather: missing plugin context")
	}
	if pc.HTTPClient == nil {
		return fmt.Errorf("weather: plugin requires permission %s", bot.PermNetwork)
	}
	p.log = pc.Logger
	p.http = pc.HTTPClient
	p.store = pc.Storage
	p.cfg = pc.Config

	p.geocodeURL = p.cfg.String("geocode_url", defaultGeocodeURL)
	p.forecastURL = p.cfg.String("forecast_url", defaultForecastURL)
	p.apiKey = p.cfg.String("api_key", "")
	p.apiHeader = p.cfg.String("api_key_header", "Authorization")
	p.apiPrefix = p.cfg.String("api_key_prefix", "Bearer ")
	p.timeout = p.cfg.Duration("timeout", defaultTimeout)

	reg.OnCommand("weather", p.onCommand, bot.WithPriority(100), bot.WithID("weather:command"))
	reg.OnRegex(`^天气[:：\s]+(\S+)\s*$`, p.onRegex, bot.WithPriority(90), bot.WithID("weather:regex"))
	reg.OnAll(p.onFollowUp, bot.WithPriority(-100), bot.WithID("weather:followup"))
	return nil
}

// Start 无后台任务。
func (p *Plugin) Start(context.Context) error { return nil }

// Stop 无资源需要释放。
func (p *Plugin) Stop(context.Context) error { return nil }

// onCommand 处理 /weather <城市>；缺少城市时进入多轮对话。
func (p *Plugin) onCommand(ctx context.Context, e *bot.Event, r bot.Reply) error {
	city := ""
	if e.Command != nil {
		city = strings.TrimSpace(strings.Join(e.Command.Args, " "))
	}
	return p.answer(ctx, e, r, city)
}

// onRegex 处理「天气 城市」，城市取自正则捕获组。
func (p *Plugin) onRegex(ctx context.Context, e *bot.Event, r bot.Reply) error {
	city := ""
	if rt, ok := bot.RouteFrom(ctx); ok && len(rt.RegexMatches) > 1 {
		city = strings.TrimSpace(rt.RegexMatches[1])
	}
	return p.answer(ctx, e, r, city)
}

// onFollowUp 处理多轮对话：会话处于待输入城市状态时把下一条消息当作城市名。
func (p *Plugin) onFollowUp(ctx context.Context, e *bot.Event, r bot.Reply) error {
	if e.Command != nil {
		return nil
	}
	city := strings.TrimSpace(e.Text())
	if city == "" || p.store == nil {
		return nil
	}
	if _, err := p.store.Get(ctx, pendingKey(e)); err != nil {
		// 没有待输入状态或存储不可用时保持沉默，不干扰其它插件。
		return nil
	}
	if err := p.store.Delete(ctx, pendingKey(e)); err != nil {
		p.log.Warn("weather: clear pending state failed", "error", err)
	}
	return p.answer(ctx, e, r, city)
}

// answer 完成一次查询或发出多轮对话追问。
func (p *Plugin) answer(ctx context.Context, e *bot.Event, r bot.Reply, city string) error {
	if city == "" {
		if err := p.store.Set(ctx, pendingKey(e), []byte{1}, pendingTTL); err != nil {
			return fmt.Errorf("weather: save pending state: %w", err)
		}
		return r.Text("请回复要查询的城市名（2 分钟内有效）").Send(ctx)
	}
	text, err := p.query(ctx, city)
	if err != nil {
		p.log.Warn("weather query failed", "city", city, "error", err)
		return r.Text("查询失败：" + err.Error()).Send(ctx)
	}
	return r.Text(text).Send(ctx)
}

// query 依次调用地理编码与天气接口，返回可直接回复的文本。
func (p *Plugin) query(ctx context.Context, city string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	place, err := p.geocode(ctx, city)
	if err != nil {
		return "", err
	}
	report, err := p.forecast(ctx, place)
	if err != nil {
		return "", err
	}
	return place.display() + " " + report, nil
}

type place struct {
	name      string
	admin1    string
	country   string
	latitude  float64
	longitude float64
}

func (p place) display() string {
	parts := make([]string, 0, 2)
	if p.admin1 != "" && p.admin1 != p.name {
		parts = append(parts, p.admin1)
	}
	if p.country != "" {
		parts = append(parts, p.country)
	}
	if len(parts) == 0 {
		return p.name
	}
	return p.name + "（" + strings.Join(parts, " · ") + "）"
}

func (p *Plugin) geocode(ctx context.Context, city string) (place, error) {
	q := url.Values{
		"name":     {city},
		"count":    {"1"},
		"language": {"zh"},
		"format":   {"json"},
	}
	var resp struct {
		Results []struct {
			Name      string  `json:"name"`
			Admin1    string  `json:"admin1"`
			Country   string  `json:"country"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}
	if err := p.getJSON(ctx, p.geocodeURL+"?"+q.Encode(), &resp); err != nil {
		return place{}, fmt.Errorf("地理编码失败: %w", err)
	}
	if len(resp.Results) == 0 {
		return place{}, fmt.Errorf("未找到城市 %q", city)
	}
	r := resp.Results[0]
	return place{
		name:      r.Name,
		admin1:    r.Admin1,
		country:   r.Country,
		latitude:  r.Latitude,
		longitude: r.Longitude,
	}, nil
}

func (p *Plugin) forecast(ctx context.Context, at place) (string, error) {
	q := url.Values{
		"latitude":  {strconv.FormatFloat(at.latitude, 'f', 4, 64)},
		"longitude": {strconv.FormatFloat(at.longitude, 'f', 4, 64)},
		"current":   {"temperature_2m,weather_code,wind_speed_10m"},
		"timezone":  {"UTC"},
	}
	var resp struct {
		Current struct {
			Time        string  `json:"time"`
			Temperature float64 `json:"temperature_2m"`
			WeatherCode int     `json:"weather_code"`
			WindSpeed   float64 `json:"wind_speed_10m"`
		} `json:"current"`
	}
	if err := p.getJSON(ctx, p.forecastURL+"?"+q.Encode(), &resp); err != nil {
		return "", fmt.Errorf("天气查询失败: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%.1f°C %s 风速 %.1f km/h",
		resp.Current.Temperature,
		weatherText(resp.Current.WeatherCode),
		resp.Current.WindSpeed,
	)
	if ts, err := time.Parse("2006-01-02T15:04", resp.Current.Time); err == nil {
		fmt.Fprintf(&b, " · %s UTC", ts.Format("2006-01-02 15:04"))
	}
	return b.String(), nil
}

// getJSON 发起带鉴权头的 GET 请求并解析 JSON。
func (p *Plugin) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if p.apiKey != "" {
		req.Header.Set(p.apiHeader, p.apiPrefix+p.apiKey)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	return nil
}

// pendingKey 返回会话级别的多轮对话状态键。
func pendingKey(e *bot.Event) string { return pendingKeyPrefix + e.SessionKey() }

// weatherText 把 WMO 天气代码转换为中文描述。
func weatherText(code int) string {
	if s, ok := wmoCodes[code]; ok {
		return s
	}
	return fmt.Sprintf("未知天气(code=%d)", code)
}

var wmoCodes = map[int]string{
	0:  "晴",
	1:  "基本晴",
	2:  "局部多云",
	3:  "阴",
	45: "雾",
	48: "雾凇",
	51: "小毛毛雨",
	53: "毛毛雨",
	55: "大毛毛雨",
	56: "冻毛毛雨",
	57: "强冻毛毛雨",
	61: "小雨",
	63: "中雨",
	65: "大雨",
	66: "冻雨",
	67: "强冻雨",
	71: "小雪",
	73: "中雪",
	75: "大雪",
	77: "雪粒",
	80: "小阵雨",
	81: "阵雨",
	82: "强阵雨",
	85: "小阵雪",
	86: "大阵雪",
	95: "雷阵雨",
	96: "雷阵雨伴小冰雹",
	99: "雷阵雨伴大冰雹",
}

func init() {
	bot.RegisterPlugin(&Plugin{})
}
