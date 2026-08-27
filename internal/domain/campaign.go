package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

func NewCampaign(id string, draft CampaignDraft, now time.Time) (*Aggregate, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: id 不能为空", ErrInvalid)
	}
	segment, err := cleanRequired("cableSegment", draft.CableSegment, 120)
	if err != nil {
		return nil, err
	}
	insulation, err := cleanRequired("insulationStructure", draft.InsulationStructure, 120)
	if err != nil {
		return nil, err
	}
	plan, err := cleanRequired("testPlan", draft.TestPlan, 500)
	if err != nil {
		return nil, err
	}
	operator, err := cleanRequired("operator", draft.Operator, 80)
	if err != nil {
		return nil, err
	}
	if draft.RatedVoltageKV <= 0 || draft.RatedVoltageKV > 1500 {
		return nil, fmt.Errorf("%w: ratedVoltageKv 应在 0 到 1500 之间", ErrInvalid)
	}
	agg := &Aggregate{Campaign: TestCampaign{
		ID: id, CableSegment: segment, InsulationStructure: insulation,
		RatedVoltageKV: draft.RatedVoltageKV, TestPlan: plan, Operator: operator,
		Status: StatusDraft, Version: 0, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}}
	agg.transition("campaign.created", operator, StatusAwaitingCollection, now, "任务建档完成，进入待采集")
	return agg, nil
}

func (a *Aggregate) AddInitialMeasurements(items []MeasurementBatch, actor string, now time.Time) error {
	if a.Campaign.Status != StatusAwaitingCollection {
		return ErrInvalidState
	}
	if len(items) != 3 {
		return fmt.Errorf("%w: 首次采集必须一次提交 A、B、C 三相测点", ErrInvalid)
	}
	phases := map[string]bool{}
	points := map[string]bool{}
	ids := map[string]bool{}
	for i := range items {
		items[i].CampaignID = a.Campaign.ID
		items[i].IsRetest = false
		items[i].SupersedesID = ""
		items[i].CapturedAt = items[i].CapturedAt.UTC()
		if items[i].CapturedAt.IsZero() {
			items[i].CapturedAt = now.UTC()
		}
		phase, err := NormalizePhase(items[i].Phase)
		if err != nil {
			return err
		}
		items[i].Phase = phase
		if err := items[i].Validate(); err != nil {
			return err
		}
		pointKey := phase + "\x00" + strings.ToLower(strings.TrimSpace(items[i].SensorPoint))
		if phases[phase] || points[pointKey] || ids[items[i].ID] {
			return fmt.Errorf("%w: 测点、相位或测量 ID 重复", ErrInvalid)
		}
		phases[phase], points[pointKey], ids[items[i].ID] = true, true, true
	}
	if !phases["A"] || !phases["B"] || !phases["C"] {
		return fmt.Errorf("%w: A、B、C 三相测点不完整", ErrInvalid)
	}
	a.Measurements = append(a.Measurements, items...)
	a.transition("measurements.captured", actor, StatusCollected, now, "三相测点采集完成")
	return nil
}

func (a *Aggregate) ApplyAssessment(findings []AssessmentFinding, actor string, now time.Time) error {
	if a.Campaign.Status != StatusCollected {
		return ErrInvalidState
	}
	if len(findings) != 3 {
		return fmt.Errorf("%w: 每个首次测量都必须有评估结果", ErrInvalid)
	}
	seen := map[string]bool{}
	needRetest := false
	for i := range findings {
		f := &findings[i]
		if f.ID == "" || f.CampaignID != a.Campaign.ID || f.MeasurementID == "" {
			return fmt.Errorf("%w: 评估结果标识不完整", ErrInvalid)
		}
		if seen[f.MeasurementID] || a.measurementByID(f.MeasurementID) == nil {
			return fmt.Errorf("%w: 评估关联测量不存在或重复", ErrInvalid)
		}
		seen[f.MeasurementID] = true
		f.CreatedAt = now.UTC()
		if f.Severity == SeverityNormal {
			f.Disposition = DispositionClosed
		} else {
			f.Disposition = DispositionOpen
			needRetest = true
		}
	}
	a.Findings = append(a.Findings, findings...)
	to := StatusAwaitingReview
	detail := "评估未发现需复验问题"
	if needRetest {
		to = StatusAwaitingRetest
		detail = "评估发现异常，等待复验"
	}
	a.transition("assessment.completed", actor, to, now, detail)
	return nil
}

func (a *Aggregate) AddRetest(retest MeasurementBatch, findingID string, resolution FindingDisposition, explanation, actor string, now time.Time) error {
	if a.Campaign.Status != StatusAwaitingRetest {
		return ErrInvalidState
	}
	finding := a.findingByID(findingID)
	if finding == nil || finding.Disposition == DispositionClosed {
		return fmt.Errorf("%w: 待复验问题不存在", ErrNotFound)
	}
	original := a.measurementByID(finding.MeasurementID)
	if original == nil || original.IsRetest {
		return fmt.Errorf("%w: 原测量不存在", ErrNotFound)
	}
	if retest.ID == "" || a.measurementByID(retest.ID) != nil {
		return fmt.Errorf("%w: 复验 ID 为空或重复", ErrInvalid)
	}
	retest.CampaignID = a.Campaign.ID
	retest.IsRetest = true
	retest.SupersedesID = original.ID
	retest.Phase = original.Phase
	if retest.CapturedAt.IsZero() {
		retest.CapturedAt = now.UTC()
	}
	if strings.TrimSpace(retest.SensorModel) == "" && strings.TrimSpace(retest.RetestCondition) == "" {
		return fmt.Errorf("%w: 必须登记替代传感器或复测条件", ErrInvalid)
	}
	if err := retest.Validate(); err != nil {
		return err
	}
	if resolution != DispositionClosed && resolution != DispositionEscalated {
		return fmt.Errorf("%w: 复验结论无效", ErrInvalid)
	}
	a.Measurements = append(a.Measurements, retest)
	finding.Disposition = resolution
	finding.ResolvedByRetest = retest.ID
	if strings.TrimSpace(explanation) != "" {
		finding.Explanation = finding.Explanation + "；复验：" + strings.TrimSpace(explanation)
	}
	to := StatusAwaitingReview
	detail := "所有异常测点已复验，进入专家审核"
	if a.HasUnresolvedFindings() {
		to = StatusAwaitingRetest
		detail = "复验问题升级或仍有其他问题待处理"
	}
	a.transition("retest.recorded", actor, to, now, detail)
	return nil
}

func (a *Aggregate) SubmitReview(review ReviewDecision, actor string, now time.Time) error {
	if a.Campaign.Status != StatusAwaitingReview && a.Campaign.Status != StatusAwaitingRetest {
		return ErrInvalidState
	}
	if review.ID == "" || strings.TrimSpace(review.Reviewer) == "" {
		return fmt.Errorf("%w: 审核标识和专家不能为空", ErrInvalid)
	}
	if review.Decision != ReviewApprove && review.Decision != ReviewRework {
		return fmt.Errorf("%w: 审核结论无效", ErrInvalid)
	}
	if review.SignatureDigest == "" {
		return fmt.Errorf("%w: 复核签名不能为空", ErrInvalid)
	}
	open := a.OpenFindings()
	if review.Decision == ReviewApprove && len(open) > 0 {
		return ErrUnresolved
	}
	if len(a.Findings) > 0 && len(review.Findings) == 0 {
		return fmt.Errorf("%w: 必须提交逐项审核意见", ErrInvalid)
	}
	known := map[string]bool{}
	for _, f := range a.Findings {
		known[f.ID] = true
	}
	for _, opinion := range review.Findings {
		if !known[opinion.FindingID] || strings.TrimSpace(opinion.Opinion) == "" {
			return fmt.Errorf("%w: 审核意见关联无效或内容为空", ErrInvalid)
		}
	}
	if review.Decision == ReviewRework && strings.TrimSpace(review.RemediationNote) == "" {
		return fmt.Errorf("%w: 退回整改必须填写整改要求", ErrInvalid)
	}
	review.CampaignID = a.Campaign.ID
	review.ReviewedAt = now.UTC()
	a.Reviews = append(a.Reviews, review)
	to := StatusApproved
	detail := "专家审核通过，允许冻结"
	if review.Decision == ReviewRework {
		to = StatusAwaitingRetest
		detail = "专家退回整改"
		for i := range a.Findings {
			if a.Findings[i].Severity != SeverityNormal {
				a.Findings[i].Disposition = DispositionOpen
			}
		}
	}
	a.transition("review.submitted", actor, to, now, detail)
	return nil
}

func (a *Aggregate) Freeze(actor string, now time.Time) (string, error) {
	if a.Campaign.Status != StatusApproved {
		return "", ErrInvalidState
	}
	if a.HasUnresolvedFindings() || len(a.Reviews) == 0 {
		return "", ErrUnresolved
	}
	digest, err := a.ComputeEvidenceDigest()
	if err != nil {
		return "", err
	}
	a.EvidenceDigest = digest
	a.transition("evidence.frozen", actor, StatusFrozen, now, "完整证据清单已冻结")
	a.FrozenVersion = a.Campaign.Version
	return digest, nil
}

func (a *Aggregate) IssueCredential(credential ReleaseCredential, actor string, now time.Time) error {
	if a.Campaign.Status != StatusFrozen || a.EvidenceDigest == "" {
		return ErrInvalidState
	}
	if credential.ID == "" || credential.CredentialCode == "" || strings.TrimSpace(credential.IssuedBy) == "" {
		return fmt.Errorf("%w: 凭据签发字段不完整", ErrInvalid)
	}
	if credential.CampaignID != a.Campaign.ID || credential.CampaignVersion != a.FrozenVersion || credential.EvidenceDigest != a.EvidenceDigest {
		return fmt.Errorf("%w: 凭据未绑定冻结证据", ErrInvalid)
	}
	credential.IssuedAt = now.UTC()
	a.Credential = &credential
	a.transition("credential.issued", actor, StatusReleased, now, "放行凭据已签发")
	return nil
}

func (a *Aggregate) transition(action, actor string, to CampaignStatus, now time.Time, detail string) {
	from := a.Campaign.Status
	a.Campaign.Status = to
	a.Campaign.Version++
	a.Campaign.UpdatedAt = now.UTC()
	a.Audit = append(a.Audit, AuditRecord{
		Sequence: len(a.Audit) + 1, Action: action, Actor: strings.TrimSpace(actor), At: now.UTC(),
		FromState: string(from), ToState: string(to), Version: a.Campaign.Version, Detail: detail,
	})
}

func (a *Aggregate) measurementByID(id string) *MeasurementBatch {
	for i := range a.Measurements {
		if a.Measurements[i].ID == id {
			return &a.Measurements[i]
		}
	}
	return nil
}

func (a *Aggregate) findingByID(id string) *AssessmentFinding {
	for i := range a.Findings {
		if a.Findings[i].ID == id {
			return &a.Findings[i]
		}
	}
	return nil
}

func (a *Aggregate) OpenFindings() []AssessmentFinding {
	var result []AssessmentFinding
	for _, finding := range a.Findings {
		if finding.Disposition == DispositionOpen || finding.Disposition == DispositionEscalated {
			result = append(result, finding)
		}
	}
	return result
}

func (a *Aggregate) HasUnresolvedFindings() bool {
	return len(a.OpenFindings()) > 0
}

type evidenceMeasurement struct {
	ID, SensorPoint, Phase, Unit, WaveformDigest, SupersedesID, SensorModel, RetestCondition string
	PeakPCC, RepetitionRate, NoiseFloor                                                      float64
	CapturedAt                                                                               time.Time
	IsRetest                                                                                 bool
}

type evidenceFinding struct {
	ID, MeasurementID, Severity, Disposition, Explanation, Recommendation, ResolvedByRetest string
	RuleCodes                                                                               []string
}

type evidenceReview struct {
	ID, Reviewer, Decision, RemediationNote, SignatureDigest string
	Findings                                                 []ReviewFinding
	ReviewedAt                                               time.Time
}

func (a *Aggregate) CanonicalEvidence() ([]byte, error) {
	measurements := make([]evidenceMeasurement, 0, len(a.Measurements))
	for _, m := range a.Measurements {
		measurements = append(measurements, evidenceMeasurement{
			ID: m.ID, SensorPoint: m.SensorPoint, Phase: m.Phase, PeakPCC: m.PeakPCC,
			RepetitionRate: m.RepetitionRate, NoiseFloor: m.NoiseFloor, Unit: m.ApparentChargeUnit,
			WaveformDigest: m.WaveformDigest, CapturedAt: m.CapturedAt.UTC(), IsRetest: m.IsRetest,
			SupersedesID: m.SupersedesID, SensorModel: m.SensorModel, RetestCondition: m.RetestCondition,
		})
	}
	sort.Slice(measurements, func(i, j int) bool { return measurements[i].ID < measurements[j].ID })
	findings := make([]evidenceFinding, 0, len(a.Findings))
	for _, f := range a.Findings {
		codes := append([]string(nil), f.RuleCodes...)
		sort.Strings(codes)
		findings = append(findings, evidenceFinding{
			ID: f.ID, MeasurementID: f.MeasurementID, Severity: string(f.Severity), RuleCodes: codes,
			Disposition: string(f.Disposition), Explanation: f.Explanation, Recommendation: f.Recommendation,
			ResolvedByRetest: f.ResolvedByRetest,
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	reviews := make([]evidenceReview, 0, len(a.Reviews))
	for _, r := range a.Reviews {
		opinions := append([]ReviewFinding(nil), r.Findings...)
		sort.Slice(opinions, func(i, j int) bool { return opinions[i].FindingID < opinions[j].FindingID })
		reviews = append(reviews, evidenceReview{ID: r.ID, Reviewer: r.Reviewer, Decision: string(r.Decision), Findings: opinions, RemediationNote: r.RemediationNote, SignatureDigest: r.SignatureDigest, ReviewedAt: r.ReviewedAt.UTC()})
	}
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].ID < reviews[j].ID })
	payload := struct {
		Campaign struct {
			ID, CableSegment, InsulationStructure, TestPlan, Operator string
			RatedVoltageKV                                            float64
		}
		Measurements []evidenceMeasurement
		Findings     []evidenceFinding
		Reviews      []evidenceReview
	}{Measurements: measurements, Findings: findings, Reviews: reviews}
	payload.Campaign.ID = a.Campaign.ID
	payload.Campaign.CableSegment = a.Campaign.CableSegment
	payload.Campaign.InsulationStructure = a.Campaign.InsulationStructure
	payload.Campaign.RatedVoltageKV = a.Campaign.RatedVoltageKV
	payload.Campaign.TestPlan = a.Campaign.TestPlan
	payload.Campaign.Operator = a.Campaign.Operator
	return json.Marshal(payload)
}

func (a *Aggregate) ComputeEvidenceDigest() (string, error) {
	b, err := a.CanonicalEvidence()
	if err != nil {
		return "", fmt.Errorf("生成证据摘要: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (a *Aggregate) Clone() (*Aggregate, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	var clone Aggregate
	if err := json.Unmarshal(b, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}
