package domain

import (
	"errors"
	"testing"
	"time"
)

func testAggregate(t *testing.T) *Aggregate {
	t.Helper()
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	agg, err := NewCampaign("cmp_test", CampaignDraft{CableSegment: "220kV 测试段", InsulationStructure: "XLPE", RatedVoltageKV: 220, TestPlan: "分级升压采集", Operator: "张试验"}, now)
	if err != nil {
		t.Fatal(err)
	}
	return agg
}

func threeMeasurements() []MeasurementBatch {
	return []MeasurementBatch{
		{ID: "m-a", SensorPoint: "A 相终端", Phase: "A", PeakPCC: 20, RepetitionRate: 100, NoiseFloor: 3, ApparentChargeUnit: "pC", WaveformDigest: "a"},
		{ID: "m-b", SensorPoint: "B 相终端", Phase: "B", PeakPCC: 21, RepetitionRate: 110, NoiseFloor: 3, ApparentChargeUnit: "pC", WaveformDigest: "b"},
		{ID: "m-c", SensorPoint: "C 相终端", Phase: "C", PeakPCC: 19, RepetitionRate: 90, NoiseFloor: 3, ApparentChargeUnit: "pC", WaveformDigest: "c"},
	}
}

func TestCampaignRequiresCompleteUniquePhases(t *testing.T) {
	agg := testAggregate(t)
	items := threeMeasurements()
	items[2].Phase = "B"
	if err := agg.AddInitialMeasurements(items, "张试验", time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("期望测点完整性错误，得到 %v", err)
	}
	if agg.Campaign.Version != 1 || agg.Campaign.Status != StatusAwaitingCollection {
		t.Fatalf("失败操作不应改变任务: %#v", agg.Campaign)
	}
}

func TestFreezeProducesStableEvidenceAndLocksState(t *testing.T) {
	agg := testAggregate(t)
	now := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	if err := agg.AddInitialMeasurements(threeMeasurements(), "张试验", now); err != nil {
		t.Fatal(err)
	}
	findings := []AssessmentFinding{
		{ID: "f-a", CampaignID: agg.Campaign.ID, MeasurementID: "m-a", Severity: SeverityNormal, RuleCodes: []string{"WITHIN_LIMITS"}},
		{ID: "f-b", CampaignID: agg.Campaign.ID, MeasurementID: "m-b", Severity: SeverityNormal, RuleCodes: []string{"WITHIN_LIMITS"}},
		{ID: "f-c", CampaignID: agg.Campaign.ID, MeasurementID: "m-c", Severity: SeverityNormal, RuleCodes: []string{"WITHIN_LIMITS"}},
	}
	if err := agg.ApplyAssessment(findings, "规则引擎", now); err != nil {
		t.Fatal(err)
	}
	reviewFindings := []ReviewFinding{{FindingID: "f-a", Opinion: "正常", Resolved: true}, {FindingID: "f-b", Opinion: "正常", Resolved: true}, {FindingID: "f-c", Opinion: "正常", Resolved: true}}
	if err := agg.SubmitReview(ReviewDecision{ID: "r-1", Reviewer: "李专家", Decision: ReviewApprove, Findings: reviewFindings, SignatureDigest: "digest"}, "李专家", now); err != nil {
		t.Fatal(err)
	}
	before, err := agg.ComputeEvidenceDigest()
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := agg.Freeze("王质量", now)
	if err != nil {
		t.Fatal(err)
	}
	if before != frozen || len(frozen) != 64 {
		t.Fatalf("证据摘要不稳定: %q / %q", before, frozen)
	}
	if agg.FrozenVersion != agg.Campaign.Version || agg.Campaign.Status != StatusFrozen {
		t.Fatalf("冻结版本或状态错误: %#v", agg.Campaign)
	}
	if err := agg.AddInitialMeasurements(threeMeasurements(), "张试验", now); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("冻结后不应允许采集: %v", err)
	}
}

func TestReviewCannotApproveOpenFinding(t *testing.T) {
	agg := testAggregate(t)
	now := time.Now()
	if err := agg.AddInitialMeasurements(threeMeasurements(), "张试验", now); err != nil {
		t.Fatal(err)
	}
	findings := []AssessmentFinding{
		{ID: "f-a", CampaignID: agg.Campaign.ID, MeasurementID: "m-a", Severity: SeverityCritical, RuleCodes: []string{"PEAK_CRITICAL"}},
		{ID: "f-b", CampaignID: agg.Campaign.ID, MeasurementID: "m-b", Severity: SeverityNormal},
		{ID: "f-c", CampaignID: agg.Campaign.ID, MeasurementID: "m-c", Severity: SeverityNormal},
	}
	if err := agg.ApplyAssessment(findings, "规则引擎", now); err != nil {
		t.Fatal(err)
	}
	err := agg.SubmitReview(ReviewDecision{ID: "r", Reviewer: "李专家", Decision: ReviewApprove, Findings: []ReviewFinding{{FindingID: "f-a", Opinion: "拟通过"}}, SignatureDigest: "sig"}, "李专家", now)
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("期望未解决问题阻断审核，得到 %v", err)
	}
}
