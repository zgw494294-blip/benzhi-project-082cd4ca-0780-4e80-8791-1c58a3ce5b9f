package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type CampaignStatus string

const (
	StatusDraft              CampaignStatus = "draft"
	StatusAwaitingCollection CampaignStatus = "awaiting_collection"
	StatusCollected          CampaignStatus = "collected"
	StatusAwaitingRetest     CampaignStatus = "awaiting_retest"
	StatusAwaitingReview     CampaignStatus = "awaiting_review"
	StatusApproved           CampaignStatus = "approved"
	StatusFrozen             CampaignStatus = "frozen"
	StatusReleased           CampaignStatus = "released"
)

type Severity string

const (
	SeverityNormal   Severity = "normal"
	SeverityWatch    Severity = "watch"
	SeverityCritical Severity = "critical"
)

type FindingDisposition string

const (
	DispositionOpen      FindingDisposition = "open"
	DispositionClosed    FindingDisposition = "closed"
	DispositionEscalated FindingDisposition = "escalated"
)

type ReviewOutcome string

const (
	ReviewApprove ReviewOutcome = "approve"
	ReviewRework  ReviewOutcome = "rework"
)

var (
	ErrInvalid        = errors.New("业务数据无效")
	ErrInvalidState   = errors.New("当前任务状态不允许此操作")
	ErrVersion        = errors.New("任务版本冲突")
	ErrNotFound       = errors.New("记录不存在")
	ErrUnresolved     = errors.New("仍有未解决问题")
	ErrEvidenceLocked = errors.New("试验证据已冻结")
)

type TestCampaign struct {
	ID                  string         `json:"id"`
	CableSegment        string         `json:"cableSegment"`
	InsulationStructure string         `json:"insulationStructure"`
	RatedVoltageKV      float64        `json:"ratedVoltageKv"`
	TestPlan            string         `json:"testPlan"`
	Operator            string         `json:"operator"`
	Status              CampaignStatus `json:"status"`
	Version             int64          `json:"version"`
	CreatedAt           time.Time      `json:"createdAt"`
	UpdatedAt           time.Time      `json:"updatedAt"`
}

type MeasurementBatch struct {
	ID                 string    `json:"id"`
	CampaignID         string    `json:"campaignID"`
	SensorPoint        string    `json:"sensorPoint"`
	Phase              string    `json:"phase"`
	PeakPCC            float64   `json:"peakPcc"`
	RepetitionRate     float64   `json:"repetitionRate"`
	NoiseFloor         float64   `json:"noiseFloor"`
	ApparentChargeUnit string    `json:"apparentChargeUnit"`
	WaveformDigest     string    `json:"waveformDigest"`
	CapturedAt         time.Time `json:"capturedAt"`
	IsRetest           bool      `json:"isRetest"`
	SupersedesID       string    `json:"supersedesID,omitempty"`
	SensorModel        string    `json:"sensorModel,omitempty"`
	RetestCondition    string    `json:"retestCondition,omitempty"`
}

type AssessmentFinding struct {
	ID               string             `json:"id"`
	CampaignID       string             `json:"campaignID"`
	MeasurementID    string             `json:"measurementID"`
	Severity         Severity           `json:"severity"`
	RuleCodes        []string           `json:"ruleCodes"`
	Explanation      string             `json:"explanation"`
	Recommendation   string             `json:"recommendation"`
	Disposition      FindingDisposition `json:"disposition"`
	ResolvedByRetest string             `json:"resolvedByRetest,omitempty"`
	CreatedAt        time.Time          `json:"createdAt"`
}

type ReviewFinding struct {
	FindingID string `json:"findingID"`
	Opinion   string `json:"opinion"`
	Resolved  bool   `json:"resolved"`
}

type ReviewDecision struct {
	ID              string          `json:"id"`
	CampaignID      string          `json:"campaignID"`
	Reviewer        string          `json:"reviewer"`
	Decision        ReviewOutcome   `json:"decision"`
	Findings        []ReviewFinding `json:"findings"`
	RemediationNote string          `json:"remediationNote"`
	ReviewedAt      time.Time       `json:"reviewedAt"`
	SignatureDigest string          `json:"signatureDigest"`
}

type ReleaseCredential struct {
	ID              string     `json:"id"`
	CampaignID      string     `json:"campaignID"`
	CampaignVersion int64      `json:"campaignVersion"`
	EvidenceDigest  string     `json:"evidenceDigest"`
	CredentialCode  string     `json:"credentialCode"`
	IssuedBy        string     `json:"issuedBy"`
	IssuedAt        time.Time  `json:"issuedAt"`
	RevokedAt       *time.Time `json:"revokedAt,omitempty"`
}

type AuditRecord struct {
	Sequence  int       `json:"sequence"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	At        time.Time `json:"at"`
	FromState string    `json:"fromState"`
	ToState   string    `json:"toState"`
	Version   int64     `json:"version"`
	Detail    string    `json:"detail"`
}

type Aggregate struct {
	Campaign       TestCampaign        `json:"campaign"`
	Measurements   []MeasurementBatch  `json:"measurements"`
	Findings       []AssessmentFinding `json:"findings"`
	Reviews        []ReviewDecision    `json:"reviews"`
	Credential     *ReleaseCredential  `json:"credential,omitempty"`
	EvidenceDigest string              `json:"evidenceDigest,omitempty"`
	FrozenVersion  int64               `json:"frozenVersion,omitempty"`
	Audit          []AuditRecord       `json:"audit"`
}

type CampaignDraft struct {
	CableSegment        string  `json:"cableSegment"`
	InsulationStructure string  `json:"insulationStructure"`
	RatedVoltageKV      float64 `json:"ratedVoltageKv"`
	TestPlan            string  `json:"testPlan"`
	Operator            string  `json:"operator"`
}

func cleanRequired(name, value string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%w: %s 不能为空", ErrInvalid, name)
	}
	if len([]rune(value)) > max {
		return "", fmt.Errorf("%w: %s 超过 %d 字", ErrInvalid, name, max)
	}
	return value, nil
}

func NormalizePhase(phase string) (string, error) {
	phase = strings.ToUpper(strings.TrimSpace(phase))
	if phase != "A" && phase != "B" && phase != "C" {
		return "", fmt.Errorf("%w: phase 必须是 A、B 或 C", ErrInvalid)
	}
	return phase, nil
}

func (m MeasurementBatch) Validate() error {
	if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.CampaignID) == "" {
		return fmt.Errorf("%w: 测量标识不完整", ErrInvalid)
	}
	if _, err := cleanRequired("sensorPoint", m.SensorPoint, 80); err != nil {
		return err
	}
	if _, err := NormalizePhase(m.Phase); err != nil {
		return err
	}
	if m.PeakPCC < 0 || m.PeakPCC > 100000 {
		return fmt.Errorf("%w: peakPcc 超出 0 到 100000", ErrInvalid)
	}
	if m.RepetitionRate < 0 || m.RepetitionRate > 1000000 {
		return fmt.Errorf("%w: repetitionRate 超出范围", ErrInvalid)
	}
	if m.NoiseFloor < 0 || m.NoiseFloor > 100000 {
		return fmt.Errorf("%w: noiseFloor 超出范围", ErrInvalid)
	}
	if m.PeakPCC < m.NoiseFloor {
		return fmt.Errorf("%w: peakPcc 不得低于 noiseFloor", ErrInvalid)
	}
	if strings.TrimSpace(m.WaveformDigest) == "" {
		return fmt.Errorf("%w: waveformDigest 不能为空", ErrInvalid)
	}
	if m.IsRetest && strings.TrimSpace(m.SupersedesID) == "" {
		return fmt.Errorf("%w: 复验必须关联原测量", ErrInvalid)
	}
	return nil
}
