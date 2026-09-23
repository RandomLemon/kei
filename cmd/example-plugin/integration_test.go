package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/RandomLemon/kei/internal/pluginmgr/external"
	"github.com/RandomLemon/kei/internal/pluginmgr/grpcsrv"
	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/proto/pluginpb"
)

const (
	coreToken = "core-token-1"
	plugName  = "example"
)

// fakeBot 是核心侧的 bot.BotAPI 假实现，只记录发送调用。
type fakeBot struct {
	mu     sync.Mutex
	sends  []*bot.Message
	target []bot.Target
}

func (f *fakeBot) Send(_ context.Context, target bot.Target, msg *bot.Message) (*bot.SendResult, error) {
	f.mu.Lock()
	f.sends = append(f.sends, msg)
	f.target = append(f.target, target)
	f.mu.Unlock()
	return &bot.SendResult{MessageID: "mid-e2e"}, nil
}

func (f *fakeBot) Reply(ctx context.Context, ev *bot.Event, msg *bot.Message) (*bot.SendResult, error) {
	return f.Send(ctx, bot.TargetFromEvent(ev), msg)
}

func (f *fakeBot) Logger() *slog.Logger { return testLogger() }

func (f *fakeBot) Storage() bot.Storage { return nil }

// snapshot 返回已发送的消息副本。
func (f *fakeBot) snapshot() ([]*bot.Message, []bot.Target) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*bot.Message(nil), f.sends...), append([]bot.Target(nil), f.target...)
}

// coreHarness 是一个已启动的核心 BotService 与挂在其上的外部插件代理。
type coreHarness struct {
	bot      *fakeBot
	reg      *captureRegistrar
	coreConn *grpc.ClientConn
}

// startHarness 启动核心服务端、示例插件进程（同进程内）以及核心侧的外部插件代理。
//
// 全程使用真实 localhost TCP，不使用 bufconn，以覆盖真实的监听、拨号与握手路径。
func startHarness(t *testing.T, coreTLS *tls.Config, coreCreds credentials.TransportCredentials) *coreHarness {
	t.Helper()

	fb := &fakeBot{}
	core, err := grpcsrv.New(grpcsrv.Options{
		Addr: "127.0.0.1:0",
		Tokens: map[string]grpcsrv.TokenInfo{
			coreToken: {Plugin: plugName, Permissions: []bot.Permission{bot.PermSendMessage}},
		},
		Bot:    fb,
		Logger: testLogger(),
		TLS:    coreTLS,
	})
	if err != nil {
		t.Fatalf("grpcsrv.New: %v", err)
	}
	if err := core.Start(); err != nil {
		t.Fatalf("grpcsrv.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := core.Stop(ctx); err != nil {
			t.Errorf("grpcsrv.Stop: %v", err)
		}
	})

	// 复用 main.go 的拨号路径，确保被测的连接构造与生产一致。
	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	coreConn, err := dialCore(dctx, core.Addr(), coreToken, coreCreds)
	if err != nil {
		t.Fatalf("dialCore: %v", err)
	}
	t.Cleanup(func() { _ = coreConn.Close() })

	p, err := newPlugin(options{
		name:     plugName,
		greeting: "问候",
		token:    coreToken,
		coreAddr: core.Addr(),
		coreConn: coreConn,
		logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("newPlugin: %v", err)
	}
	pluginAddr := servePlugin(t, p)

	ext, err := external.New(external.Config{
		Name:        plugName,
		Addr:        pluginAddr,
		CoreAddr:    core.Addr(),
		Token:       coreToken,
		Timeout:     5 * time.Second,
		Permissions: []bot.Permission{bot.PermSendMessage},
	}, external.Deps{Logger: testLogger()})
	if err != nil {
		t.Fatalf("external.New: %v", err)
	}
	t.Cleanup(func() { _ = ext.Close() })

	reg := &captureRegistrar{}
	if err := ext.Setup(context.Background(), reg); err != nil {
		t.Fatalf("external.Setup: %v", err)
	}
	return &coreHarness{bot: fb, reg: reg, coreConn: coreConn}
}

// servePlugin 在随机端口上暴露插件的 PluginService，返回实际监听地址。
func servePlugin(t *testing.T, p *plugin) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("插件监听: %v", err)
	}
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, p)
	// Serve 在 Stop/GracefulStop 之后返回，该 goroutine 必然退出。
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.GracefulStop()
		_ = ln.Close()
	})
	return ln.Addr().String()
}

// commandEvent 构造一条带命令的消息事件。
func commandEvent(text string) *bot.Event {
	return &bot.Event{
		ID:       "evt-e2e-1",
		Type:     bot.EventMessage,
		Platform: "mock",
		BotID:    "bot-a",
		Time:     time.Now().UTC(),
		Message: &bot.Message{
			ID:       "m-1",
			Kind:     bot.MessageGroup,
			Segments: []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}}},
		},
		Sender:  &bot.User{ID: "u-1", Name: "张三"},
		Channel: &bot.Channel{ID: "room-1", Name: "房间", Kind: bot.MessageGroup},
		Command: bot.ParseCommand(text, []string{"/"}),
	}
}

// dispatch 通过核心侧注册的兜底 Handler 投递事件。
func (h *coreHarness) dispatch(t *testing.T, ev *bot.Event) {
	t.Helper()
	if len(h.reg.all) != 1 {
		t.Fatalf("兜底 Handler 数 = %d，期望 1", len(h.reg.all))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.reg.all[0](ctx, ev, nil); err != nil {
		t.Fatalf("投递事件: %v", err)
	}
}

// captureRegistrar 记录外部插件代理注册的兜底 Handler。
type captureRegistrar struct {
	all []bot.Handler
}

func (r *captureRegistrar) OnCommand(string, bot.Handler, ...bot.Option) {}
func (r *captureRegistrar) OnRegex(string, bot.Handler, ...bot.Option)   {}
func (r *captureRegistrar) OnKeyword([]string, bot.Handler, ...bot.Option) {
}
func (r *captureRegistrar) OnEvent(bot.EventType, bot.Handler, ...bot.Option) {}
func (r *captureRegistrar) Use(bot.Middleware)                                {}
func (r *captureRegistrar) OnAll(h bot.Handler, _ ...bot.Option) {
	r.all = append(r.all, h)
}

func TestEndToEndCommandRoundTrip(t *testing.T) {
	h := startHarness(t, nil, insecure.NewCredentials())

	// /ext 之后的参数是回复内容，核心侧的假 BotAPI 必须收到 <greeting>: hi。
	h.dispatch(t, commandEvent("/ext hi"))

	msgs, targets := h.bot.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("核心收到发送次数 = %d，期望 1", len(msgs))
	}
	if got := msgs[0].PlainText(); got != "问候: hi" {
		t.Fatalf("回复文本 = %q，期望 %q", got, "问候: hi")
	}
	if targets[0].ChannelID != "room-1" || targets[0].Platform != "mock" || targets[0].UserID != "u-1" {
		t.Fatalf("发送目标 = %+v，期望从事件回填", targets[0])
	}
	if msgs[0].Kind != bot.MessageGroup {
		t.Fatalf("回复消息 Kind = %q，期望 group", msgs[0].Kind)
	}

	// 未命中的事件不应产生任何发送，也不能报错。
	h.dispatch(t, commandEvent("无关内容"))
	if msgs, _ := h.bot.snapshot(); len(msgs) != 1 {
		t.Fatalf("未命中事件产生了额外发送，总次数 = %d", len(msgs))
	}
}

func TestEndToEndRejectsBadToken(t *testing.T) {
	fb := &fakeBot{}
	core, err := grpcsrv.New(grpcsrv.Options{
		Addr:   "127.0.0.1:0",
		Tokens: map[string]grpcsrv.TokenInfo{coreToken: {Plugin: plugName}},
		Bot:    fb,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("grpcsrv.New: %v", err)
	}
	if err := core.Start(); err != nil {
		t.Fatalf("grpcsrv.Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = core.Stop(ctx)
	}()

	// 插件进程侧的令牌与核心下发的不一致：Init 必须被拒绝，New 必须失败。
	coreConn, err := dialCore(context.Background(), core.Addr(), coreToken, insecure.NewCredentials())
	if err != nil {
		t.Fatalf("dialCore: %v", err)
	}
	defer func() { _ = coreConn.Close() }()

	p, err := newPlugin(options{
		name:     plugName,
		greeting: "问候",
		token:    "different-token",
		coreConn: coreConn,
		logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("newPlugin: %v", err)
	}
	pluginAddr := servePlugin(t, p)

	_, err = external.New(external.Config{
		Name:     plugName,
		Addr:     pluginAddr,
		CoreAddr: core.Addr(),
		Token:    coreToken,
		Timeout:  5 * time.Second,
	}, external.Deps{Logger: testLogger()})
	if err == nil {
		t.Fatal("令牌不匹配时 external.New 应失败")
	}
	if !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("错误 = %v，期望包含插件返回的 bad token", err)
	}
}

func TestEndToEndBadTokenBlocksSendMessage(t *testing.T) {
	h := startHarness(t, nil, insecure.NewCredentials())

	// 核心只认 coreToken；插件若拿着别的令牌发消息，必须被拒绝而不是静默丢弃。
	_, err := pluginpb.NewBotServiceClient(h.coreConn).SendMessage(
		context.Background(),
		&pluginpb.SendRequest{
			Token:   "wrong",
			Target:  &pluginpb.Target{Platform: "mock", ChannelId: "room-1"},
			Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"x"}`}}},
		})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("错误令牌发送 code = %v，期望 Unauthenticated", status.Code(err))
	}
	if msgs, _ := h.bot.snapshot(); len(msgs) != 0 {
		t.Fatalf("错误令牌不应产生发送，实际 %d 次", len(msgs))
	}
}

// tlsFiles 是一组写入临时目录的 PEM 文件路径。
type tlsFiles struct {
	caCert, caKey        string
	serverCert, srvKey   string
	clientCert, cliKey   string
	pool                 *x509.CertPool
	serverTLS, clientTLS *tls.Config
}

// makeTLSFiles 生成自签 CA、服务端证书与客户端证书，全部写入 t.TempDir()。
//
// 服务端证书同时带 IP 与 DNS SAN：核心监听在 127.0.0.1，客户端按 target 推导
// ServerName 得到 "127.0.0.1"，只有 IP SAN 才能通过校验。
func makeTLSFiles(t *testing.T) *tlsFiles {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 私钥: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kei-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("签发 CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("解析 CA: %v", err)
	}

	f := &tlsFiles{
		caCert: filepath.Join(dir, "ca.pem"),
		caKey:  filepath.Join(dir, "ca-key.pem"),
	}
	writePEM(t, f.caCert, "CERTIFICATE", caDER)
	writeKeyPEM(t, f.caKey, caKey)
	f.pool = x509.NewCertPool()
	f.pool.AddCert(caCert)

	serverCert, serverKey := issueCert(t, "server", caCert, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, true)
	f.serverCert = filepath.Join(dir, "server.pem")
	f.srvKey = filepath.Join(dir, "server-key.pem")
	writePEM(t, f.serverCert, "CERTIFICATE", serverCert.Raw)
	writeKeyPEM(t, f.srvKey, serverKey)

	clientCert, clientKey := issueCert(t, "client", caCert, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, false)
	f.clientCert = filepath.Join(dir, "client.pem")
	f.cliKey = filepath.Join(dir, "client-key.pem")
	writePEM(t, f.clientCert, "CERTIFICATE", clientCert.Raw)
	writeKeyPEM(t, f.cliKey, clientKey)

	serverPair, err := tls.LoadX509KeyPair(f.serverCert, f.srvKey)
	if err != nil {
		t.Fatalf("加载服务端证书: %v", err)
	}
	f.serverTLS = &tls.Config{
		Certificates: []tls.Certificate{serverPair},
		ClientCAs:    f.pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}

	clientPair, err := tls.LoadX509KeyPair(f.clientCert, f.cliKey)
	if err != nil {
		t.Fatalf("加载客户端证书: %v", err)
	}
	f.clientTLS = &tls.Config{
		RootCAs:      f.pool,
		Certificates: []tls.Certificate{clientPair},
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS12,
	}
	return f
}

// issueCert 用 CA 签发一张叶证书。
func issueCert(t *testing.T, cn string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, usage []x509.ExtKeyUsage, server bool) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 %s 私钥: %v", cn, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usage,
	}
	if server {
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.DNSNames = []string{"localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("签发 %s 证书: %v", cn, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析 %s 证书: %v", cn, err)
	}
	return cert, key
}

// writePEM 把证书 DER 写入文件。
func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatalf("写入 %s: %v", path, err)
	}
}

// writeKeyPEM 把私钥写入文件。
func writeKeyPEM(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("编码私钥: %v", err)
	}
	writePEM(t, path, "EC PRIVATE KEY", der)
}

func TestEndToEndMutualTLS(t *testing.T) {
	f := makeTLSFiles(t)

	// 插件侧同样走 main.go 的证书加载路径，验证 -tls-ca/-tls-cert/-tls-key 的真实效果。
	creds, err := coreCredentials(f.caCert, f.clientCert, f.cliKey)
	if err != nil {
		t.Fatalf("coreCredentials: %v", err)
	}
	h := startHarness(t, f.serverTLS, creds)

	h.dispatch(t, commandEvent("/ext mTLS"))
	msgs, _ := h.bot.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("mTLS 链路下核心收到发送次数 = %d，期望 1", len(msgs))
	}
	if got := msgs[0].PlainText(); got != "问候: mTLS" {
		t.Fatalf("回复文本 = %q，期望 %q", got, "问候: mTLS")
	}
}

func TestMutualTLSRejectsClientWithoutCert(t *testing.T) {
	f := makeTLSFiles(t)
	fb := &fakeBot{}
	core, err := grpcsrv.New(grpcsrv.Options{
		Addr:   "127.0.0.1:0",
		Tokens: map[string]grpcsrv.TokenInfo{coreToken: {Plugin: plugName, Permissions: []bot.Permission{bot.PermSendMessage}}},
		Bot:    fb,
		Logger: testLogger(),
		TLS:    f.serverTLS,
	})
	if err != nil {
		t.Fatalf("grpcsrv.New: %v", err)
	}
	if err := core.Start(); err != nil {
		t.Fatalf("grpcsrv.Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = core.Stop(ctx)
	}()

	// 信任 CA 但不出示客户端证书：RequireAndVerifyClientCert 必须拒绝握手。
	conn, err := grpc.NewClient(core.Addr(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs:    f.pool,
			ServerName: "localhost",
			MinVersion: tls.VersionTLS12,
		})),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = pluginpb.NewBotServiceClient(conn).SendMessage(ctx, &pluginpb.SendRequest{
		Token:   coreToken,
		Target:  &pluginpb.Target{Platform: "mock", ChannelId: "room-1"},
		Message: &pluginpb.Message{Segments: []*pluginpb.Segment{{Type: string(bot.SegText), DataJson: `{"text":"x"}`}}},
	})
	if err == nil {
		t.Fatal("缺少客户端证书时握手应失败")
	}
	if msgs, _ := fb.snapshot(); len(msgs) != 0 {
		t.Fatalf("握手失败不应产生发送，实际 %d 次", len(msgs))
	}
}

// listenLine 匹配插件启动日志中的监听地址。
var listenLine = regexp.MustCompile(`listen=(\S+)`)

// coreReadyLine 匹配插件成功连上核心的日志。
var coreReadyLine = regexp.MustCompile(`已连接核心`)

// buildPluginBinary 构建示例插件二进制，返回其可执行文件路径。
func buildPluginBinary(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "example-plugin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 example-plugin: %v\n%s", err, out)
	}
	return bin
}

// pluginProcess 是一个已启动的示例插件子进程，日志按行流式读取。
type pluginProcess struct {
	cmd    *exec.Cmd
	addr   string
	lines  chan string
	waitCh chan error
	stop   chan struct{}

	// mu 保护 waited：清理函数与等待 goroutine 会并发触碰它。
	mu     sync.Mutex
	waited bool
}

// markWaited 记录进程已经 Wait 完成。
func (p *pluginProcess) markWaited() {
	p.mu.Lock()
	p.waited = true
	p.mu.Unlock()
}

// exited 报告进程是否已经 Wait 完成。
func (p *pluginProcess) exited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waited
}

// startPluginProcess 启动插件子进程，等待其打印监听地址后返回句柄。
//
// 通过日志读取真实监听地址，因此 -listen 可以传 127.0.0.1:0 交给内核分配，
// 避免测试事先抢占端口带来的竞态。
func startPluginProcess(t *testing.T, bin, coreAddr string, extra ...string) *pluginProcess {
	t.Helper()

	args := append([]string{
		"-listen", "127.0.0.1:0",
		"-core-addr", coreAddr,
		"-token", coreToken,
		"-name", plugName,
		"-greeting", "问候",
	}, extra...)
	cmd := exec.Command(bin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动插件进程: %v", err)
	}

	p := &pluginProcess{
		cmd:    cmd,
		lines:  make(chan string, 512),
		waitCh: make(chan error, 1),
		stop:   make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		p.markWaited()
		p.waitCh <- err
	}()
	// 持续读取日志：进程退出后管道关闭，或 stop 被关闭时该 goroutine 退出。
	// 用 select 发送是必要的——否则测试失败后无人消费 lines 时它会永久阻塞。
	go func() {
		defer close(p.lines)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			select {
			case p.lines <- scanner.Text():
			case <-p.stop:
				return
			}
		}
	}()

	t.Cleanup(func() {
		close(p.stop)
		if p.exited() {
			return
		}
		// 进程仍在运行时杀掉它；Wait goroutine 会随之结束并向带缓冲的
		// waitCh 投递结果后退出，不会泄漏。
		_ = p.cmd.Process.Kill()
	})

	p.addr = p.waitForLine(t, listenLine)
	return p
}

// waitForLine 阻塞直到出现匹配 re 的日志行，返回第一个捕获组。
func (p *pluginProcess) waitForLine(t *testing.T, re *regexp.Regexp) string {
	t.Helper()

	timeout := time.After(30 * time.Second)
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				t.Fatal("插件进程已退出，未出现预期日志")
			}
			if m := re.FindStringSubmatch(line); m != nil {
				if len(m) > 1 {
					return m[1]
				}
				return line
			}
		case err := <-p.waitCh:
			t.Fatalf("插件进程提前退出: %v", err)
		case <-timeout:
			t.Fatal("等待插件日志超时")
		}
	}
}

// awaitCoreReady 阻塞直到插件确认已连上核心。
func (p *pluginProcess) awaitCoreReady(t *testing.T) {
	t.Helper()
	p.waitForLine(t, coreReadyLine)
}

// wait 等待进程退出并返回其退出状态。
func (p *pluginProcess) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-p.waitCh:
		return err
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatal("等待插件进程退出超时")
		return nil
	}
}

// interrupt 向进程发送 SIGINT。
func (p *pluginProcess) interrupt(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("发送 SIGINT: %v", err)
	}
}

// startCore 启动核心侧 BotService，并用 t.Cleanup 回收。
func startCore(t *testing.T, fb *fakeBot, addr string) *grpcsrv.Server {
	t.Helper()

	core, err := grpcsrv.New(grpcsrv.Options{
		Addr: addr,
		Tokens: map[string]grpcsrv.TokenInfo{
			coreToken: {Plugin: plugName, Permissions: []bot.Permission{bot.PermSendMessage}},
		},
		Bot:    fb,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("grpcsrv.New: %v", err)
	}
	if err := core.Start(); err != nil {
		t.Fatalf("grpcsrv.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = core.Stop(ctx)
	})
	return core
}

func TestExamplePluginBinaryEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("需要构建二进制，-short 模式跳过")
	}

	bin := buildPluginBinary(t)
	fb := &fakeBot{}
	core := startCore(t, fb, "127.0.0.1:0")

	proc := startPluginProcess(t, bin, core.Addr())
	proc.awaitCoreReady(t)

	ext, err := external.New(external.Config{
		Name:        plugName,
		Addr:        proc.addr,
		CoreAddr:    core.Addr(),
		Token:       coreToken,
		Timeout:     5 * time.Second,
		Permissions: []bot.Permission{bot.PermSendMessage},
	}, external.Deps{Logger: testLogger()})
	if err != nil {
		t.Fatalf("连接真实插件进程: %v", err)
	}
	t.Cleanup(func() { _ = ext.Close() })

	reg := &captureRegistrar{}
	if err := ext.Setup(context.Background(), reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reg.all[0](ctx, commandEvent("/ext 真实进程"), nil); err != nil {
		t.Fatalf("投递事件: %v", err)
	}

	msgs, _ := fb.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("核心收到发送次数 = %d，期望 1", len(msgs))
	}
	if got := msgs[0].PlainText(); got != "问候: 真实进程" {
		t.Fatalf("回复文本 = %q，期望 %q", got, "问候: 真实进程")
	}

	// 优雅退出：Stop 会先通知插件 Shutdown，再关闭连接。
	if err := ext.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// SIGINT 必须让插件进程自行优雅退出（GracefulStop 后返回 0），而不是被杀。
	proc.interrupt(t)
	if err := proc.wait(t); err != nil {
		t.Fatalf("插件进程退出状态 = %v，期望正常退出", err)
	}
}

// TestExamplePluginStartsBeforeCore 覆盖「插件先启动、核心后启动」的顺序。
//
// 这是两侧互相等待缺陷的回归测试：插件必须能在核心尚未监听时就完成监听并
// 暴露 PluginService，核心随后起来才能 dial 成功、完成 Init 并收发消息。
func TestExamplePluginStartsBeforeCore(t *testing.T) {
	if testing.Short() {
		t.Skip("需要构建二进制，-short 模式跳过")
	}

	bin := buildPluginBinary(t)

	// 核心地址预留后立即释放：此刻确实无人监听，插件必须仍然能启动，
	// 而不是因为连不上核心而退出。
	coreAddr := freeTCPAddr(t)
	proc := startPluginProcess(t, bin, coreAddr)

	// 到这里插件已经完成监听，核心才启动——正是原先会死锁的顺序。
	fb := &fakeBot{}
	core := startCore(t, fb, coreAddr)

	// 核心起来后，插件侧对核心的连接会自动变为 Ready。
	proc.awaitCoreReady(t)

	// 核心 dial 插件并完成 Init；若插件仍在等核心才监听，这一步会 connection refused。
	ext, err := external.New(external.Config{
		Name:        plugName,
		Addr:        proc.addr,
		CoreAddr:    core.Addr(),
		Token:       coreToken,
		Timeout:     5 * time.Second,
		Permissions: []bot.Permission{bot.PermSendMessage},
	}, external.Deps{Logger: testLogger()})
	if err != nil {
		t.Fatalf("核心后启动时 external.New 应成功: %v", err)
	}
	t.Cleanup(func() { _ = ext.Close() })

	reg := &captureRegistrar{}
	if err := ext.Setup(context.Background(), reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reg.all[0](ctx, commandEvent("/ext 后起核心"), nil); err != nil {
		t.Fatalf("投递事件: %v", err)
	}

	msgs, _ := fb.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("核心收到发送次数 = %d，期望 1", len(msgs))
	}
	if got := msgs[0].PlainText(); got != "问候: 后起核心" {
		t.Fatalf("回复文本 = %q，期望 %q", got, "问候: 后起核心")
	}

	proc.interrupt(t)
	if err := proc.wait(t); err != nil {
		t.Fatalf("插件进程退出状态 = %v，期望正常退出", err)
	}
}

// freeTCPAddr 返回一个刚释放的 localhost 地址，供「稍后监听」的场景使用。
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return addr
}

func TestExamplePluginBinaryFailsWithoutRequiredFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("需要构建二进制，-short 模式跳过")
	}

	bin := buildPluginBinary(t)

	// 缺少必填参数必须以非零码退出，并说明缺了什么。
	out, err := exec.Command(bin, "-listen", "127.0.0.1:0").CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("缺少必填参数应非零退出，实际 err = %v，输出 = %s", err, out)
	}
	if !strings.Contains(string(out), "-core-addr") || !strings.Contains(string(out), "-token") {
		t.Fatalf("错误输出未列出缺失参数: %s", out)
	}
}
