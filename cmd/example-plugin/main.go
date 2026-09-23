// Command example-plugin 是一个用 Go 编写的外部插件示例。
//
// 它演示了外部插件的最小完整形态：
//  1. 监听自身地址并暴露 PluginService 供核心调用；
//  2. 以客户端身份连接核心的 BotService（携带令牌）；
//  3. 在 HandleEvent 中判定事件，并通过 BotService.SendMessage 让核心代为发送回复。
//
// 启动顺序：插件先 net.Listen 并开始 Serve，然后才等待核心就绪。这个顺序是
// 确定的——核心启动时会 dial 插件并在 Init 上等待，若插件反过来先等核心，
// 两侧会互相等待，任一侧重启都会卡死。因此推荐「先启动插件进程，再启动核心」；
// 由于核心侧 Init 带 WaitForReady、插件侧对核心的连接也在核心起来后自动变为
// Ready，两侧同时拉起或任一侧先起同样能成功。
//
// 运行方式（核心侧需在 configs 中为该插件配置 grpc_addr 指向 -listen 的地址）：
//
//	example-plugin -listen 127.0.0.1:9100 -core-addr 127.0.0.1:9000 -token s3cret
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/RandomLemon/kei/proto/pluginpb"
)

// tokenHeader 是插件反向调用核心时携带令牌的元数据键。
//
// 令牌本身写在每个请求体内（grpcsrv 以请求体为准校验），元数据是额外的
// 传输层标记，便于核心侧的拦截器/网关做统一审计。
const tokenHeader = "x-plugin-token"

// dialTimeout 是启动阶段连接核心的超时，超时即视为启动失败。
const dialTimeout = 10 * time.Second

// main 解析参数、建立双向连接并阻塞直到收到退出信号。
func main() {
	if err := run(); err != nil {
		slog.Error("外部插件启动失败", "err", err)
		os.Exit(1)
	}
}

// run 完成插件的启动与优雅退出流程。
func run() error {
	fs := flag.NewFlagSet("example-plugin", flag.ContinueOnError)
	listen := fs.String("listen", "", "插件自身 gRPC 监听地址，例如 127.0.0.1:9100（必填）")
	coreAddr := fs.String("core-addr", "", "核心 BotService 地址，例如 127.0.0.1:9000（必填）")
	token := fs.String("token", "", "核心分配的插件令牌（必填）")
	name := fs.String("name", "example-plugin", "插件名，需与核心配置一致")
	greeting := fs.String("greeting", "你好，我是外部插件", "回复前缀，最终消息形如 <greeting>: <内容>")
	tlsCA := fs.String("tls-ca", "", "连接核心时校验用的 CA 证书文件（可选）")
	tlsCert := fs.String("tls-cert", "", "连接核心时的客户端证书文件（可选）")
	tlsKey := fs.String("tls-key", "", "连接核心时的客户端私钥文件（可选）")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if missing := missingFlags(
		flagValue{"-listen", *listen},
		flagValue{"-core-addr", *coreAddr},
		flagValue{"-token", *token},
	); len(missing) > 0 {
		return fmt.Errorf("缺少必填参数 %v", missing)
	}

	creds, err := coreCredentials(*tlsCA, *tlsCert, *tlsKey)
	if err != nil {
		return err
	}

	// 启动顺序（关键）：先监听并暴露 PluginService，再等待核心就绪。
	//
	// 若反过来「先等核心 Ready 再监听」，两侧会互相等待：核心启动时要 dial
	// 插件并在 Init 上等待，插件却在等核心的 BotService 起来，只有极窄的时间
	// 窗口能成功，任一侧重启都会卡死。先监听则顺序确定：核心可以立刻 dial 到
	// 插件并投递 Init，插件侧对核心的连接会在核心起来后自动变 Ready。
	//
	// 因此推荐「先启动插件进程，再启动核心」；两侧同时拉起或任一侧先起也都能成功。
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", *listen, err)
	}

	// 连接核心的对象在此创建，但 grpc.NewClient 是惰性的、不会阻塞：
	// 真正的握手发生在下面的 waitCore 阶段，那时插件已经在监听。
	coreConn, err := newCoreClient(*coreAddr, *token, creds)
	if err != nil {
		return err
	}
	defer func() { _ = coreConn.Close() }()

	p, err := newPlugin(options{
		name:     *name,
		greeting: *greeting,
		token:    *token,
		coreAddr: *coreAddr,
		coreConn: coreConn,
		logger:   logger,
	})
	if err != nil {
		return err
	}

	// 插件自身的 PluginService 监听保持明文：mTLS 的强制点在核心侧
	// （核心校验插件证书），插件只需出示证书即可，无需双向强制。
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, p)

	serveErr := make(chan error, 1)
	go func() {
		// Serve 在 GracefulStop/Stop 之后返回，该 goroutine 因此必然退出。
		serveErr <- srv.Serve(ln)
	}()

	logger.Info("外部插件已监听", "listen", ln.Addr().String(), "core_addr", *coreAddr, "tls", *tlsCA != "")

	// 插件已在监听，核心随时可以 dial 过来；这里再等核心就绪，作为
	// 「-core-addr 配错」的快速失败。
	dctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	if err := waitCore(dctx, coreConn); err != nil {
		srv.Stop()
		<-serveErr
		return fmt.Errorf("等待核心 %s 就绪: %w", *coreAddr, err)
	}
	logger.Info("已连接核心", "core_addr", *coreAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("gRPC 服务异常退出: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅停止")
		srv.GracefulStop()
		<-serveErr
		return nil
	}
}

// flagValue 是一个待校验的命令行参数。
type flagValue struct {
	// name 是参数名，用于错误信息。
	name string
	// value 是参数值，空串表示缺失。
	value string
}

// missingFlags 返回值为空的参数名，顺序与入参一致以保证报错可复现。
func missingFlags(values ...flagValue) []string {
	missing := make([]string, 0, len(values))
	for _, v := range values {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	return missing
}

// newCoreClient 创建连接核心 BotService 的客户端，并为每次 RPC 附加令牌元数据。
//
// grpc.NewClient 是惰性的：它不建立连接也不阻塞，握手推迟到第一次 RPC 或
// waitCore，因此本函数可以在插件开始监听之前安全调用。
func newCoreClient(addr, token string, creds credentials.TransportCredentials) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithUnaryInterceptor(tokenInterceptor(token)),
	)
	if err != nil {
		return nil, fmt.Errorf("构造核心客户端 %s: %w", addr, err)
	}
	return conn, nil
}

// dialCore 创建连接核心的客户端并阻塞等待其就绪，供调用方一次性完成连接。
//
// 返回的连接由调用方负责关闭。
func dialCore(ctx context.Context, addr, token string, creds credentials.TransportCredentials) (*grpc.ClientConn, error) {
	conn, err := newCoreClient(addr, token, creds)
	if err != nil {
		return nil, err
	}
	if err := waitCore(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("连接核心 %s: %w", addr, err)
	}
	return conn, nil
}

// waitCore 阻塞直到连接进入 Ready 状态或 ctx 结束。
//
// 用于把「-core-addr 配错/核心未启动」在启动阶段暴露，而不是等到第一次
// 事件投递才失败。
func waitCore(ctx context.Context, conn *grpc.ClientConn) error {
	conn.Connect()
	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return nil
		}
		if !conn.WaitForStateChange(ctx, state) {
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("连接状态停留于 %s", state)
		}
	}
}

// tokenInterceptor 返回把令牌写入出站元数据的 unary 拦截器。
func tokenInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(metadata.AppendToOutgoingContext(ctx, tokenHeader, token), method, req, reply, cc, opts...)
	}
}

// coreCredentials 依据证书参数构造连接核心的传输凭据。
//
// 三项都未提供时返回明文凭据；只要提供了任意一项，就必须三项齐全，
// 因为客户端证书与 CA 在校验服务端时互为前提。
func coreCredentials(caFile, certFile, keyFile string) (credentials.TransportCredentials, error) {
	if caFile == "" && certFile == "" && keyFile == "" {
		return insecure.NewCredentials(), nil
	}
	if missing := missingFlags(
		flagValue{"-tls-ca", caFile},
		flagValue{"-tls-cert", certFile},
		flagValue{"-tls-key", keyFile},
	); len(missing) > 0 {
		return nil, fmt.Errorf("TLS 参数必须同时提供，缺少 %v", missing)
	}

	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("读取 CA 证书: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA 证书 %s 不含有效证书", caFile)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("加载客户端证书: %w", err)
	}
	return credentials.NewTLS(&tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}), nil
}
