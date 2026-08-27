package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pdreview/internal/application"
	"pdreview/internal/assessment"
	"pdreview/internal/domain"
	"pdreview/internal/store"
	"pdreview/internal/web"
)

const defaultAddress = "127.0.0.1:19081"

type config struct {
	address   string
	dataDir   string
	selfCheck bool
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败："+err.Error())
		os.Exit(1)
	}
}

func run(arguments []string, getenv func(string) string, stdout, stderr io.Writer) error {
	cfg, err := parseConfig(arguments, getenv, stderr)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if cfg.selfCheck {
		return runSelfCheck(cfg, logger, stdout)
	}
	return runServer(cfg, logger)
}

func parseConfig(arguments []string, getenv func(string) string, stderr io.Writer) (config, error) {
	set := flag.NewFlagSet("pd-review", flag.ContinueOnError)
	set.SetOutput(stderr)
	address := set.String("addr", defaultAddress, "HTTP 监听地址")
	dataDir := set.String("data-dir", "data", "事件账本与快照目录")
	selfCheck := set.Bool("self-check", false, "执行有界端到端自检后退出")
	if err := set.Parse(arguments); err != nil {
		return config{}, err
	}
	if set.NArg() != 0 {
		return config{}, fmt.Errorf("不支持位置参数: %s", strings.Join(set.Args(), " "))
	}
	addressExplicit := false
	set.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			addressExplicit = true
		}
	})
	if !addressExplicit {
		port := strings.TrimSpace(getenv("PORT"))
		if port != "" {
			number, err := strconv.Atoi(port)
			if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
				return config{}, fmt.Errorf("PORT 必须是 1 到 65535 的十进制端口号")
			}
			*address = net.JoinHostPort("127.0.0.1", port)
		}
	}
	if err := validateAddress(*address); err != nil {
		return config{}, err
	}
	if strings.TrimSpace(*dataDir) == "" {
		return config{}, fmt.Errorf("data-dir 不能为空")
	}
	return config{address: *address, dataDir: *dataDir, selfCheck: *selfCheck}, nil
}

func validateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("addr 必须是 host:port 格式: %w", err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("addr 必须明确指定监听主机")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("addr 端口必须在 1 到 65535 之间")
	}
	return nil
}

func buildApplication(dataDir string, logger *slog.Logger) (*store.Ledger, http.Handler, error) {
	ledger, err := store.Open(dataDir)
	if err != nil {
		return nil, nil, err
	}
	service, err := application.NewService(ledger, assessment.MustDefault())
	if err != nil {
		_ = ledger.Close()
		return nil, nil, err
	}
	server, err := web.NewServer(service, logger)
	if err != nil {
		_ = ledger.Close()
		return nil, nil, err
	}
	return ledger, server.Handler(), nil
}

func runServer(cfg config, logger *slog.Logger) error {
	ledger, handler, err := buildApplication(cfg.dataDir, logger)
	if err != nil {
		return err
	}
	defer ledger.Close()
	listener, err := net.Listen("tcp", cfg.address)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", cfg.address, err)
	}
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}
	serveErrors := make(chan error, 1)
	go func() {
		logger.Info("局部放电放行工作台已启动", "address", listener.Addr().String(), "dataDir", cfg.dataDir)
		serveErrors <- server.Serve(listener)
	}()
	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP 服务异常退出: %w", err)
		}
		return nil
	case <-signalContext.Done():
		logger.Info("收到关闭信号，正在刷新账本")
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("有序关闭 HTTP 服务: %w", err)
	}
	if err := ledger.Flush(); err != nil {
		return err
	}
	return nil
}

func runSelfCheck(cfg config, logger *slog.Logger, stdout io.Writer) error {
	tempDir, err := os.MkdirTemp("", "pd-review-self-check-")
	if err != nil {
		return fmt.Errorf("创建自检目录: %w", err)
	}
	defer os.RemoveAll(tempDir)
	ledger, handler, err := buildApplication(filepath.Join(tempDir, "ledger"), logger)
	if err != nil {
		return err
	}
	defer ledger.Close()
	listener, err := net.Listen("tcp", cfg.address)
	if err != nil {
		return fmt.Errorf("自检监听 %s: %w", cfg.address, err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	baseURL := "http://" + listener.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}
	if err := executeSmokeFlow(client, baseURL); err != nil {
		_ = server.Close()
		return err
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("关闭自检服务: %w", err)
	}
	if err := <-serveErrors; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("自检 HTTP 服务退出异常: %w", err)
	}
	if err := ledger.Flush(); err != nil {
		return err
	}
	reopened, err := store.Open(filepath.Join(tempDir, "ledger"))
	if err != nil {
		return fmt.Errorf("重启重放校验失败: %w", err)
	}
	if err := reopened.Close(); err != nil {
		return fmt.Errorf("关闭重放账本: %w", err)
	}
	fmt.Fprintf(stdout, "自检通过：建档、采集、评估、复验、审核、冻结、凭据签发与验证链路完成；监听地址 %s；事件账本可重放。\n", cfg.address)
	return nil
}

type smokeEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func executeSmokeFlow(client *http.Client, baseURL string) error {
	var aggregate domain.Aggregate
	if err := smokeRequest(client, http.MethodGet, baseURL+"/healthz", "", nil, nil); err != nil {
		return err
	}
	draft := domain.CampaignDraft{CableSegment: "自检电缆段", InsulationStructure: "XLPE 预制接头", RatedVoltageKV: 220, TestPlan: "1.7U0 分级升压并采集三相相位图谱", Operator: "自检试验员"}
	if err := smokeRequest(client, http.MethodPost, baseURL+"/api/campaigns", "smoke-create", draft, &aggregate); err != nil {
		return err
	}
	id := aggregate.Campaign.ID
	capture := application.CaptureMeasurementsCommand{ExpectedVersion: aggregate.Campaign.Version, Actor: "自检试验员", Measurements: []application.MeasurementInput{
		{SensorPoint: "A相终端", Phase: "A", PeakPCC: 150, RepetitionRate: 1200, NoiseFloor: 5, ApparentChargeUnit: "pC", WaveformDigest: "wave-a"},
		{SensorPoint: "B相终端", Phase: "B", PeakPCC: 22, RepetitionRate: 120, NoiseFloor: 4, ApparentChargeUnit: "pC", WaveformDigest: "wave-b"},
		{SensorPoint: "C相终端", Phase: "C", PeakPCC: 20, RepetitionRate: 110, NoiseFloor: 4, ApparentChargeUnit: "pC", WaveformDigest: "wave-c"},
	}}
	if err := smokeRequest(client, http.MethodPost, baseURL+"/api/campaigns/"+id+"/measurements", "smoke-capture", capture, &aggregate); err != nil {
		return err
	}
	if err := smokeRequest(client, http.MethodPost, baseURL+"/api/campaigns/"+id+"/assessments", "smoke-assess", application.AssessCommand{ExpectedVersion: aggregate.Campaign.Version, Actor: "自检试验员"}, &aggregate); err != nil {
		return err
	}
	var abnormal *domain.AssessmentFinding
	for i := range aggregate.Findings {
		if aggregate.Findings[i].Disposition == domain.DispositionOpen {
			abnormal = &aggregate.Findings[i]
			break
		}
	}
	if abnormal == nil {
		return fmt.Errorf("自检评估未生成异常问题")
	}
	retest := application.RetestCommand{ExpectedVersion: aggregate.Campaign.Version, Actor: "自检试验员", FindingID: abnormal.ID, SensorPoint: "A相替代测点", PeakPCC: 30, RepetitionRate: 90, NoiseFloor: 5, WaveformDigest: "wave-a-retest", SensorModel: "HFCT-B", RetestCondition: "重新接地并更换耦合位置"}
	if err := smokeRequest(client, http.MethodPost, baseURL+"/api/campaigns/"+id+"/retests", "smoke-retest", retest, &aggregate); err != nil {
		return err
	}
	if aggregate.Campaign.Status != domain.StatusAwaitingReview {
		return fmt.Errorf("自检复验后状态错误: %s", aggregate.Campaign.Status)
	}
	opinions := make([]domain.ReviewFinding, 0, len(aggregate.Findings))
	for _, finding := range aggregate.Findings {
		opinions = append(opinions, domain.ReviewFinding{FindingID: finding.ID, Opinion: "规则证据与复验结果已复核", Resolved: finding.Disposition == domain.DispositionClosed})
	}
	review := application.ReviewCommand{ExpectedVersion: aggregate.Campaign.Version, Reviewer: "自检诊断专家", Decision: domain.ReviewApprove, Findings: opinions, RemediationNote: "复验满足关闭条件", Signature: "self-check-expert-signature"}
	if err := smokeRequest(client, http.MethodPost, baseURL+"/api/campaigns/"+id+"/review", "smoke-review", review, &aggregate); err != nil {
		return err
	}
	if err := smokeRequest(client, http.MethodPost, baseURL+"/api/campaigns/"+id+"/freeze", "smoke-freeze", application.FreezeCommand{ExpectedVersion: aggregate.Campaign.Version, QualityOwner: "自检质量负责人"}, &aggregate); err != nil {
		return err
	}
	if len(aggregate.EvidenceDigest) != 64 {
		return fmt.Errorf("自检证据摘要长度错误")
	}
	if err := smokeRequest(client, http.MethodPost, baseURL+"/api/campaigns/"+id+"/credential", "smoke-credential", application.IssueCredentialCommand{ExpectedVersion: aggregate.Campaign.Version, IssuedBy: "自检质量负责人"}, &aggregate); err != nil {
		return err
	}
	if aggregate.Credential == nil {
		return fmt.Errorf("自检未生成放行凭据")
	}
	var verification application.CredentialVerification
	if err := smokeRequest(client, http.MethodPost, baseURL+"/api/credentials/verify", "", map[string]string{"code": aggregate.Credential.CredentialCode}, &verification); err != nil {
		return err
	}
	if !verification.Valid || !verification.FormatValid {
		return fmt.Errorf("自检凭据验证失败: %s", verification.Reason)
	}
	return nil
}

func smokeRequest(client *http.Client, method, url, idempotencyKey string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("自检请求 %s: %w", url, err)
	}
	defer response.Body.Close()
	b, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("自检请求 %s 返回 %d: %s", url, response.StatusCode, strings.TrimSpace(string(b)))
	}
	if output == nil {
		return nil
	}
	var envelope smokeEnvelope
	if err := json.Unmarshal(b, &envelope); err != nil {
		return fmt.Errorf("解析自检响应: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("自检 API 错误 %s: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if err := json.Unmarshal(envelope.Data, output); err != nil {
		return fmt.Errorf("解析自检数据: %w", err)
	}
	return nil
}
