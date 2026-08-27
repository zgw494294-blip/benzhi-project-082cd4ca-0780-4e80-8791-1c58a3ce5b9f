package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"pdreview/internal/application"
	"pdreview/internal/domain"
)

//go:embed assets/*
var assetFiles embed.FS

type Server struct {
	service *application.Service
	logger  *slog.Logger
	assets  fs.FS
}

type responseEnvelope struct {
	Data  any       `json:"data,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	CurrentVersion int64  `json:"currentVersion,omitempty"`
}

func NewServer(service *application.Service, logger *slog.Logger) (*Server, error) {
	if service == nil {
		return nil, fmt.Errorf("web 服务缺少应用服务")
	}
	if logger == nil {
		logger = slog.Default()
	}
	assets, err := fs.Sub(assetFiles, "assets")
	if err != nil {
		return nil, fmt.Errorf("装载页面资源: %w", err)
	}
	return &Server{service: service, logger: logger, assets: assets}, nil
}

func (s *Server) Handler() http.Handler {
	return s.recoverMiddleware(s.securityMiddleware(http.HandlerFunc(s.route)))
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		s.HandleWorkbench(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/assets/"):
		s.HandleAsset(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		s.HandleHealth(w, r)
	case r.URL.Path == "/api/campaigns" && r.Method == http.MethodGet:
		s.HandleListCampaigns(w, r)
	case r.URL.Path == "/api/campaigns" && r.Method == http.MethodPost:
		s.HandleCreateCampaign(w, r)
	case r.URL.Path == "/api/credentials/verify" && r.Method == http.MethodPost:
		s.HandleVerifyCredential(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/campaigns/"):
		s.routeCampaign(w, r)
	default:
		s.writeError(w, r, http.StatusNotFound, "not_found", "路由不存在", nil)
	}
}

func (s *Server) routeCampaign(w http.ResponseWriter, r *http.Request) {
	remainder := strings.TrimPrefix(r.URL.Path, "/api/campaigns/")
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodGet {
		s.HandleGetCampaign(w, r, parts[0])
		return
	}
	if len(parts) != 2 || parts[0] == "" || r.Method != http.MethodPost {
		s.writeError(w, r, http.StatusNotFound, "not_found", "任务路由不存在", nil)
		return
	}
	id := parts[0]
	switch parts[1] {
	case "measurements":
		s.HandleCaptureMeasurements(w, r, id)
	case "assessments":
		s.HandleAssess(w, r, id)
	case "retests":
		s.HandleRetest(w, r, id)
	case "review":
		s.HandleReview(w, r, id)
	case "freeze":
		s.HandleFreeze(w, r, id)
	case "credential":
		s.HandleIssueCredential(w, r, id)
	default:
		s.writeError(w, r, http.StatusNotFound, "not_found", "任务操作不存在", nil)
	}
}

func (s *Server) HandleWorkbench(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(s.assets, "index.html")
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "asset_error", "工作台资源不可用", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) HandleAsset(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	if name != "app.css" && name != "app.js" {
		s.writeError(w, r, http.StatusNotFound, "not_found", "静态资源不存在", nil)
		return
	}
	b, err := fs.ReadFile(s.assets, name)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "not_found", "静态资源不存在", err)
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if strings.HasSuffix(name, ".js") {
		contentType = "text/javascript; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}

func (s *Server) HandleListCampaigns(w http.ResponseWriter, r *http.Request) {
	items, err := s.service.ListCampaigns()
	if err != nil {
		s.writeDomainError(w, r, err, "读取任务列表失败")
		return
	}
	s.writeJSON(w, http.StatusOK, items)
}

func (s *Server) HandleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	var draft domain.CampaignDraft
	if !s.decode(w, r, &draft) {
		return
	}
	result, err := s.service.CreateCampaign(application.CreateCampaignCommand{IdempotencyKey: r.Header.Get("Idempotency-Key"), Draft: draft})
	if err != nil {
		s.writeDomainError(w, r, err, "任务建档失败")
		return
	}
	s.writeJSON(w, http.StatusCreated, result)
}

func (s *Server) HandleGetCampaign(w http.ResponseWriter, r *http.Request, id string) {
	result, err := s.service.GetCampaign(id)
	if err != nil {
		s.writeDomainError(w, r, err, "读取任务失败")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) HandleCaptureMeasurements(w http.ResponseWriter, r *http.Request, id string) {
	var command application.CaptureMeasurementsCommand
	if !s.decode(w, r, &command) {
		return
	}
	command.CampaignID = id
	command.IdempotencyKey = r.Header.Get("Idempotency-Key")
	result, err := s.service.CaptureMeasurements(command)
	if err != nil {
		s.writeDomainError(w, r, err, "测量采集失败")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) HandleAssess(w http.ResponseWriter, r *http.Request, id string) {
	var command application.AssessCommand
	if !s.decode(w, r, &command) {
		return
	}
	command.CampaignID = id
	command.IdempotencyKey = r.Header.Get("Idempotency-Key")
	result, err := s.service.Assess(command)
	if err != nil {
		s.writeDomainError(w, r, err, "风险评估失败")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) HandleRetest(w http.ResponseWriter, r *http.Request, id string) {
	var command application.RetestCommand
	if !s.decode(w, r, &command) {
		return
	}
	command.CampaignID = id
	command.IdempotencyKey = r.Header.Get("Idempotency-Key")
	result, err := s.service.Retest(command)
	if err != nil {
		s.writeDomainError(w, r, err, "复验登记失败")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) HandleReview(w http.ResponseWriter, r *http.Request, id string) {
	var command application.ReviewCommand
	if !s.decode(w, r, &command) {
		return
	}
	command.CampaignID = id
	command.IdempotencyKey = r.Header.Get("Idempotency-Key")
	result, err := s.service.Review(command)
	if err != nil {
		s.writeDomainError(w, r, err, "专家审核失败")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) HandleFreeze(w http.ResponseWriter, r *http.Request, id string) {
	var command application.FreezeCommand
	if !s.decode(w, r, &command) {
		return
	}
	command.CampaignID = id
	command.IdempotencyKey = r.Header.Get("Idempotency-Key")
	result, err := s.service.Freeze(command)
	if err != nil {
		s.writeDomainError(w, r, err, "冻结证据失败")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) HandleIssueCredential(w http.ResponseWriter, r *http.Request, id string) {
	var command application.IssueCredentialCommand
	if !s.decode(w, r, &command) {
		return
	}
	command.CampaignID = id
	command.IdempotencyKey = r.Header.Get("Idempotency-Key")
	result, err := s.service.IssueCredential(command)
	if err != nil {
		s.writeDomainError(w, r, err, "签发凭据失败")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) HandleVerifyCredential(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code string `json:"code"`
	}
	if !s.decode(w, r, &input) {
		return
	}
	result := s.service.VerifyCredential(input.Code)
	status := http.StatusOK
	if !result.FormatValid {
		status = http.StatusUnprocessableEntity
	}
	s.writeJSON(w, status, result)
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		s.writeError(w, r, http.StatusUnsupportedMediaType, "content_type", "Content-Type 必须是 application/json", nil)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "invalid_json", "请求 JSON 无效："+jsonMessage(err), err)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		s.writeError(w, r, http.StatusBadRequest, "invalid_json", "请求只能包含一个 JSON 对象", err)
		return false
	}
	return true
}

func jsonMessage(err error) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("第 %d 字节附近存在语法错误", syntax.Offset)
	}
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) {
		return fmt.Sprintf("字段 %s 的类型不正确", typeError.Field)
	}
	if strings.HasPrefix(err.Error(), "json: unknown field") {
		return strings.Replace(err.Error(), "json: unknown field", "未知字段", 1)
	}
	if errors.Is(err, io.EOF) {
		return "请求体为空"
	}
	return "无法解析请求体"
}

func (s *Server) writeDomainError(w http.ResponseWriter, r *http.Request, err error, context string) {
	status, code := http.StatusInternalServerError, "internal_error"
	message := context
	var currentVersion int64
	switch {
	case errors.Is(err, domain.ErrNotFound):
		status, code, message = http.StatusNotFound, "not_found", "记录不存在"
	case errors.Is(err, domain.ErrVersion):
		status, code, message = http.StatusConflict, "version_conflict", "任务版本已变化，请刷新后重试"
		parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/campaigns/"), "/"), "/")
		if len(parts) > 0 {
			if current, getErr := s.service.GetCampaign(parts[0]); getErr == nil {
				currentVersion = current.Campaign.Version
			}
		}
	case errors.Is(err, domain.ErrInvalidState):
		status, code, message = http.StatusConflict, "invalid_state", err.Error()
	case errors.Is(err, domain.ErrUnresolved):
		status, code, message = http.StatusUnprocessableEntity, "unresolved_findings", err.Error()
	case errors.Is(err, domain.ErrInvalid):
		status, code, message = http.StatusUnprocessableEntity, "validation_failed", err.Error()
	}
	if status >= 500 {
		s.logger.Error("HTTP 请求失败", "method", r.Method, "path", r.URL.Path, "error", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(responseEnvelope{Error: &apiError{Code: code, Message: message, CurrentVersion: currentVersion}})
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, cause error) {
	if status >= 500 {
		s.logger.Error("HTTP 请求失败", "method", r.Method, "path", r.URL.Path, "error", cause)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(responseEnvelope{Error: &apiError{Code: code, Message: message}})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(responseEnvelope{Data: data})
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("HTTP 处理器异常", "panic", recovered, "path", r.URL.Path)
				s.writeError(w, r, http.StatusInternalServerError, "internal_error", "服务处理请求时发生异常", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
