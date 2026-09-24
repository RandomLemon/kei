package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Send 实现 bot.BotAPI：把消息发送到目标会话。
//
// 发送前按适配器能力降级消息段，按 bot 维度限流，并在失败时按退避重试。
// 重试是 at-least-once 语义：对端已收到但响应失败时会重复投递。
func (e *Engine) Send(ctx context.Context, target bot.Target, msg *bot.Message) (*bot.SendResult, error) {
	if msg == nil {
		return nil, errors.New("engine: nil message")
	}
	return e.SendRequest(ctx, &bot.SendRequest{Target: target, Message: msg})
}

// SendRequest 发送一条完整的发送请求，支持引用回复（ReplyTo）。
//
// 这是 Send 的底层入口，供需要 reply_to 的调用方（例如 gRPC BotService）
// 使用；req.BotID 与 req.Target.BotID 都可指定机器人。
func (e *Engine) SendRequest(ctx context.Context, req *bot.SendRequest) (*bot.SendResult, error) {
	if req == nil {
		return nil, errors.New("engine: nil send request")
	}
	if req.Message == nil {
		return nil, errors.New("engine: nil message")
	}
	target := req.Target
	if target.BotID == "" {
		target.BotID = req.BotID
	}

	ad, botID, err := e.resolve(target)
	if err != nil {
		return nil, err
	}
	if target.BotID == "" {
		target.BotID = botID
	}
	degraded := bot.Degrade(req.Message, ad.Capabilities())
	if len(degraded.Segments) == 0 {
		// 空消息或降级后全部段被丢弃：直接报错，绝不静默发送空消息。
		return nil, fmt.Errorf("engine: 消息在按平台能力降级后没有可发送的段（bot %s, platform %s）", botID, ad.Name())
	}
	out := &bot.SendRequest{
		BotID:   botID,
		Target:  target,
		Message: degraded,
		ReplyTo: req.ReplyTo,
	}

	if lim, ok := e.sendLimits[botID]; ok && lim != nil {
		if err := lim.Wait(ctx, botID); err != nil {
			return nil, fmt.Errorf("engine: send rate limited: %w", err)
		}
	}

	attempts := e.opts.SendRetries + 1
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			timer := time.NewTimer(e.opts.SendBackoff << (attempt - 1))
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, fmt.Errorf("engine: send canceled: %w", ctx.Err())
			case <-timer.C:
			}
		}
		start := time.Now()
		res, err := ad.Send(ctx, out)
		e.met.MessageSent(ad.Name(), time.Since(start), err)
		if err == nil {
			if res == nil {
				res = &bot.SendResult{}
			}
			return res, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		e.log.Debug("send failed, retrying",
			"bot", botID,
			"platform", ad.Name(),
			"attempt", attempt+1,
			"error", err,
		)
	}
	return nil, fmt.Errorf("engine: send via %s: %w", ad.Name(), lastErr)
}

// Reply 实现 bot.BotAPI：回复事件所在会话。
func (e *Engine) Reply(ctx context.Context, ev *bot.Event, msg *bot.Message) (*bot.SendResult, error) {
	if ev == nil {
		return nil, errors.New("engine: nil event")
	}
	return e.Send(ctx, bot.TargetFromEvent(ev), msg)
}

// resolve 依据 Target 选择适配器。
//
// target.BotID 为空时，仅当该平台只有一个 bot 才自动选择；否则报错要求显式指定。
func (e *Engine) resolve(target bot.Target) (bot.Adapter, string, error) {
	if target.Platform == "" {
		return nil, "", errors.New("engine: target platform is empty")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()

	if target.BotID != "" {
		ad, ok := e.adapters[target.BotID]
		if !ok {
			return nil, "", fmt.Errorf("engine: unknown bot %q", target.BotID)
		}
		if name := ad.Name(); name != target.Platform {
			return nil, "", fmt.Errorf("engine: bot %q is on platform %q, not %q", target.BotID, name, target.Platform)
		}
		return ad, target.BotID, nil
	}

	ids := e.platforms[target.Platform]
	switch len(ids) {
	case 0:
		return nil, "", fmt.Errorf("engine: no adapter for platform %q", target.Platform)
	case 1:
		return e.adapters[ids[0]], ids[0], nil
	default:
		return nil, "", fmt.Errorf("engine: platform %q has %d bots, target.BotID is required", target.Platform, len(ids))
	}
}
