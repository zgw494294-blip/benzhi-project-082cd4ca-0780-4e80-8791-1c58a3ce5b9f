package application

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"pdreview/internal/assessment"
	"pdreview/internal/domain"
)

type Repository interface {
	Save(aggregate *domain.Aggregate, expectedPreviousVersion int64, idempotencyKey, eventType, actor string, now time.Time) (*domain.Aggregate, bool, error)
	GetCampaign(id string) (*domain.Aggregate, error)
	GetIdempotent(key string) (*domain.Aggregate, bool, error)
	ListCampaigns() ([]*domain.Aggregate, error)
	VerifyCredential(code string) (*domain.ReleaseCredential, bool, string, error)
}

type Service struct {
	repository Repository
	evaluator  *assessment.Evaluator
	now        func() time.Time
	newID      func(string) string
}

type MeasurementInput struct {
	SensorPoint        string  `json:"sensorPoint"`
	Phase              string  `json:"phase"`
	PeakPCC            float64 `json:"peakPcc"`
	RepetitionRate     float64 `json:"repetitionRate"`
	NoiseFloor         float64 `json:"noiseFloor"`
	ApparentChargeUnit string  `json:"apparentChargeUnit"`
	WaveformDigest     string  `json:"waveformDigest"`
}

type CreateCampaignCommand struct {
	IdempotencyKey string               `json:"-"`
	Draft          domain.CampaignDraft `json:"draft"`
}

type CaptureMeasurementsCommand struct {
	CampaignID      string             `json:"-"`
	ExpectedVersion int64              `json:"expectedVersion"`
	IdempotencyKey  string             `json:"-"`
	Actor           string             `json:"actor"`
	Measurements    []MeasurementInput `json:"measurements"`
}

type AssessCommand struct {
	CampaignID      string `json:"-"`
	ExpectedVersion int64  `json:"expectedVersion"`
	IdempotencyKey  string `json:"-"`
	Actor           string `json:"actor"`
}

type RetestCommand struct {
	CampaignID      string  `json:"-"`
	ExpectedVersion int64   `json:"expectedVersion"`
	IdempotencyKey  string  `json:"-"`
	Actor           string  `json:"actor"`
	FindingID       string  `json:"findingID"`
	SensorPoint     string  `json:"sensorPoint"`
	PeakPCC         float64 `json:"peakPcc"`
	RepetitionRate  float64 `json:"repetitionRate"`
	NoiseFloor      float64 `json:"noiseFloor"`
	WaveformDigest  string  `json:"waveformDigest"`
	SensorModel     string  `json:"sensorModel"`
	RetestCondition string  `json:"retestCondition"`
}

type ReviewCommand struct {
	CampaignID      string                 `json:"-"`
	ExpectedVersion int64                  `json:"expectedVersion"`
	IdempotencyKey  string                 `json:"-"`
	Reviewer        string                 `json:"reviewer"`
	Decision        domain.ReviewOutcome   `json:"decision"`
	Findings        []domain.ReviewFinding `json:"findings"`
	RemediationNote string                 `json:"remediationNote"`
	Signature       string                 `json:"signature"`
}

type FreezeCommand struct {
	CampaignID      string `json:"-"`
	ExpectedVersion int64  `json:"expectedVersion"`
	IdempotencyKey  string `json:"-"`
	QualityOwner    string `json:"qualityOwner"`
}

type IssueCredentialCommand struct {
	CampaignID      string `json:"-"`
	ExpectedVersion int64  `json:"expectedVersion"`
	IdempotencyKey  string `json:"-"`
	IssuedBy        string `json:"issuedBy"`
}

type CredentialVerification struct {
	Code           string                    `json:"code"`
	FormatValid    bool                      `json:"formatValid"`
	Valid          bool                      `json:"valid"`
	Reason         string                    `json:"reason"`
	OfflinePayload *OfflineCredentialPayload `json:"offlinePayload,omitempty"`
	Credential     *domain.ReleaseCredential `json:"credential,omitempty"`
}

type OfflineCredentialPayload struct {
	CampaignID      string `json:"campaignID"`
	CampaignVersion int64  `json:"campaignVersion"`
	EvidenceDigest  string `json:"evidenceDigest"`
	IssuedAt        string `json:"issuedAt"`
}

func NewService(repository Repository, evaluator *assessment.Evaluator) (*Service, error) {
	if repository == nil || evaluator == nil {
		return nil, fmt.Errorf("应用服务依赖不完整")
	}
	return &Service{repository: repository, evaluator: evaluator, now: time.Now, newID: randomID}, nil
}

func (s *Service) WithClock(now func() time.Time) *Service {
	if now != nil {
		s.now = now
	}
	return s
}

func (s *Service) WithIDFactory(factory func(string) string) *Service {
	if factory != nil {
		s.newID = factory
	}
	return s
}

func randomID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())))
		b = sum[:12]
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func (s *Service) idempotent(key, campaignID string) (*domain.Aggregate, bool, error) {
	if strings.TrimSpace(key) == "" {
		return nil, false, fmt.Errorf("%w: 缺少 Idempotency-Key", domain.ErrInvalid)
	}
	aggregate, found, err := s.repository.GetIdempotent(key)
	if err == nil && found && campaignID != "" && aggregate.Campaign.ID != campaignID {
		return nil, false, fmt.Errorf("幂等键已用于其他任务")
	}
	return aggregate, found, err
}

func (s *Service) CreateCampaign(command CreateCampaignCommand) (*domain.Aggregate, error) {
	if existing, found, err := s.idempotent(command.IdempotencyKey, ""); err != nil || found {
		return existing, err
	}
	now := s.now().UTC()
	aggregate, err := domain.NewCampaign(s.newID("cmp"), command.Draft, now)
	if err != nil {
		return nil, err
	}
	stored, _, err := s.repository.Save(aggregate, 0, command.IdempotencyKey, "campaign.created", command.Draft.Operator, now)
	return stored, err
}

func (s *Service) CaptureMeasurements(command CaptureMeasurementsCommand) (*domain.Aggregate, error) {
	if existing, found, err := s.idempotent(command.IdempotencyKey, command.CampaignID); err != nil || found {
		return existing, err
	}
	aggregate, err := s.loadVersion(command.CampaignID, command.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	measurements := make([]domain.MeasurementBatch, 0, len(command.Measurements))
	for _, input := range command.Measurements {
		unit := strings.TrimSpace(input.ApparentChargeUnit)
		if unit == "" {
			unit = "pC"
		}
		measurements = append(measurements, domain.MeasurementBatch{
			ID: s.newID("mea"), SensorPoint: input.SensorPoint, Phase: input.Phase,
			PeakPCC: input.PeakPCC, RepetitionRate: input.RepetitionRate, NoiseFloor: input.NoiseFloor,
			ApparentChargeUnit: unit, WaveformDigest: input.WaveformDigest, CapturedAt: now,
		})
	}
	if err := aggregate.AddInitialMeasurements(measurements, command.Actor, now); err != nil {
		return nil, err
	}
	stored, _, err := s.repository.Save(aggregate, command.ExpectedVersion, command.IdempotencyKey, "measurements.captured", command.Actor, now)
	return stored, err
}

func (s *Service) Assess(command AssessCommand) (*domain.Aggregate, error) {
	if existing, found, err := s.idempotent(command.IdempotencyKey, command.CampaignID); err != nil || found {
		return existing, err
	}
	aggregate, err := s.loadVersion(command.CampaignID, command.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	findings, err := s.evaluator.Assess(aggregate.Campaign.ID, aggregate.Measurements, func() string { return s.newID("fnd") }, now)
	if err != nil {
		return nil, err
	}
	if err := aggregate.ApplyAssessment(findings, command.Actor, now); err != nil {
		return nil, err
	}
	stored, _, err := s.repository.Save(aggregate, command.ExpectedVersion, command.IdempotencyKey, "assessment.completed", command.Actor, now)
	return stored, err
}

func (s *Service) Retest(command RetestCommand) (*domain.Aggregate, error) {
	if existing, found, err := s.idempotent(command.IdempotencyKey, command.CampaignID); err != nil || found {
		return existing, err
	}
	aggregate, err := s.loadVersion(command.CampaignID, command.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	var finding *domain.AssessmentFinding
	for i := range aggregate.Findings {
		if aggregate.Findings[i].ID == command.FindingID {
			finding = &aggregate.Findings[i]
			break
		}
	}
	if finding == nil {
		return nil, domain.ErrNotFound
	}
	var original *domain.MeasurementBatch
	for i := range aggregate.Measurements {
		if aggregate.Measurements[i].ID == finding.MeasurementID {
			original = &aggregate.Measurements[i]
			break
		}
	}
	if original == nil {
		return nil, domain.ErrNotFound
	}
	now := s.now().UTC()
	retest := domain.MeasurementBatch{
		ID: s.newID("ret"), CampaignID: aggregate.Campaign.ID, SensorPoint: command.SensorPoint,
		Phase: original.Phase, PeakPCC: command.PeakPCC, RepetitionRate: command.RepetitionRate,
		NoiseFloor: command.NoiseFloor, ApparentChargeUnit: original.ApparentChargeUnit,
		WaveformDigest: command.WaveformDigest, CapturedAt: now, IsRetest: true,
		SupersedesID: original.ID, SensorModel: command.SensorModel, RetestCondition: command.RetestCondition,
	}
	comparison, err := s.evaluator.CompareRetest(*original, retest)
	if err != nil {
		return nil, err
	}
	if err := aggregate.AddRetest(retest, command.FindingID, comparison.Disposition, comparison.Explanation, command.Actor, now); err != nil {
		return nil, err
	}
	stored, _, err := s.repository.Save(aggregate, command.ExpectedVersion, command.IdempotencyKey, "retest.recorded", command.Actor, now)
	return stored, err
}

func (s *Service) Review(command ReviewCommand) (*domain.Aggregate, error) {
	if existing, found, err := s.idempotent(command.IdempotencyKey, command.CampaignID); err != nil || found {
		return existing, err
	}
	aggregate, err := s.loadVersion(command.CampaignID, command.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	signatureDigest := sha256.Sum256([]byte(strings.TrimSpace(command.Signature)))
	review := domain.ReviewDecision{
		ID: s.newID("rev"), Reviewer: command.Reviewer, Decision: command.Decision,
		Findings: command.Findings, RemediationNote: command.RemediationNote,
		SignatureDigest: hex.EncodeToString(signatureDigest[:]), ReviewedAt: now,
	}
	if strings.TrimSpace(command.Signature) == "" {
		review.SignatureDigest = ""
	}
	if err := aggregate.SubmitReview(review, command.Reviewer, now); err != nil {
		return nil, err
	}
	stored, _, err := s.repository.Save(aggregate, command.ExpectedVersion, command.IdempotencyKey, "review.submitted", command.Reviewer, now)
	return stored, err
}

func (s *Service) Freeze(command FreezeCommand) (*domain.Aggregate, error) {
	if existing, found, err := s.idempotent(command.IdempotencyKey, command.CampaignID); err != nil || found {
		return existing, err
	}
	aggregate, err := s.loadVersion(command.CampaignID, command.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if strings.TrimSpace(command.QualityOwner) == "" {
		return nil, fmt.Errorf("%w: 质量负责人不能为空", domain.ErrInvalid)
	}
	if _, err := aggregate.Freeze(command.QualityOwner, now); err != nil {
		return nil, err
	}
	stored, _, err := s.repository.Save(aggregate, command.ExpectedVersion, command.IdempotencyKey, "evidence.frozen", command.QualityOwner, now)
	return stored, err
}

func (s *Service) IssueCredential(command IssueCredentialCommand) (*domain.Aggregate, error) {
	if existing, found, err := s.idempotent(command.IdempotencyKey, command.CampaignID); err != nil || found {
		return existing, err
	}
	aggregate, err := s.loadVersion(command.CampaignID, command.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(command.IssuedBy) == "" {
		return nil, fmt.Errorf("%w: 签发人不能为空", domain.ErrInvalid)
	}
	now := s.now().UTC()
	payload := OfflineCredentialPayload{
		CampaignID: aggregate.Campaign.ID, CampaignVersion: aggregate.FrozenVersion,
		EvidenceDigest: aggregate.EvidenceDigest, IssuedAt: now.Format(time.RFC3339),
	}
	code, err := encodeCredential(payload)
	if err != nil {
		return nil, err
	}
	credential := domain.ReleaseCredential{
		ID: s.newID("cred"), CampaignID: aggregate.Campaign.ID, CampaignVersion: aggregate.FrozenVersion,
		EvidenceDigest: aggregate.EvidenceDigest, CredentialCode: code, IssuedBy: command.IssuedBy, IssuedAt: now,
	}
	if err := aggregate.IssueCredential(credential, command.IssuedBy, now); err != nil {
		return nil, err
	}
	stored, _, err := s.repository.Save(aggregate, command.ExpectedVersion, command.IdempotencyKey, "credential.issued", command.IssuedBy, now)
	return stored, err
}

func (s *Service) GetCampaign(id string) (*domain.Aggregate, error) {
	return s.repository.GetCampaign(strings.TrimSpace(id))
}

func (s *Service) ListCampaigns() ([]*domain.Aggregate, error) {
	return s.repository.ListCampaigns()
}

func (s *Service) VerifyCredential(code string) CredentialVerification {
	result := CredentialVerification{Code: strings.TrimSpace(code)}
	payload, err := decodeCredential(result.Code)
	if err != nil {
		result.Reason = "凭据离线格式校验失败：" + err.Error()
		return result
	}
	result.FormatValid = true
	result.OfflinePayload = &payload
	credential, valid, reason, err := s.repository.VerifyCredential(result.Code)
	result.Credential = credential
	result.Valid = valid
	result.Reason = reason
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		result.Valid = false
		result.Reason = "凭据查询失败：" + err.Error()
	}
	if credential != nil && (payload.CampaignID != credential.CampaignID || payload.CampaignVersion != credential.CampaignVersion || payload.EvidenceDigest != credential.EvidenceDigest) {
		result.Valid = false
		result.Reason = "离线载荷与账本凭据不一致"
	}
	return result
}

func (s *Service) loadVersion(campaignID string, expected int64) (*domain.Aggregate, error) {
	if strings.TrimSpace(campaignID) == "" || expected < 1 {
		return nil, fmt.Errorf("%w: campaignID 或 expectedVersion 无效", domain.ErrInvalid)
	}
	aggregate, err := s.repository.GetCampaign(campaignID)
	if err != nil {
		return nil, err
	}
	if aggregate.Campaign.Version != expected {
		return nil, domain.ErrVersion
	}
	return aggregate, nil
}

func encodeCredential(payload OfflineCredentialPayload) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码凭据载荷: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte("PD-RELEASE-V1|" + encoded))
	return "PD1." + encoded + "." + hex.EncodeToString(sum[:8]), nil
}

func decodeCredential(code string) (OfflineCredentialPayload, error) {
	var payload OfflineCredentialPayload
	parts := strings.Split(strings.TrimSpace(code), ".")
	if len(parts) != 3 || parts[0] != "PD1" {
		return payload, fmt.Errorf("前缀或分段数量无效")
	}
	sum := sha256.Sum256([]byte("PD-RELEASE-V1|" + parts[1]))
	if parts[2] != hex.EncodeToString(sum[:8]) {
		return payload, fmt.Errorf("校验码不匹配")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return payload, fmt.Errorf("载荷不是有效 Base64URL")
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, fmt.Errorf("载荷 JSON 无效")
	}
	if payload.CampaignID == "" || payload.CampaignVersion < 1 || len(payload.EvidenceDigest) != 64 {
		return payload, fmt.Errorf("载荷字段不完整")
	}
	if _, err := time.Parse(time.RFC3339, payload.IssuedAt); err != nil {
		return payload, fmt.Errorf("签发时间无效")
	}
	return payload, nil
}
