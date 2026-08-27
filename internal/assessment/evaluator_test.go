package assessment

import (
	"testing"
	"time"

	"pdreview/internal/domain"
)

func TestAssessmentStableRulesAndPhaseDominance(t *testing.T) {
	e := MustDefault()
	measurements := []domain.MeasurementBatch{
		{ID: "a", CampaignID: "c", SensorPoint: "A", Phase: "A", PeakPCC: 150, RepetitionRate: 1200, NoiseFloor: 5, ApparentChargeUnit: "pC", WaveformDigest: "a"},
		{ID: "b", CampaignID: "c", SensorPoint: "B", Phase: "B", PeakPCC: 20, RepetitionRate: 100, NoiseFloor: 4, ApparentChargeUnit: "pC", WaveformDigest: "b"},
		{ID: "c", CampaignID: "c", SensorPoint: "C", Phase: "C", PeakPCC: 18, RepetitionRate: 90, NoiseFloor: 4, ApparentChargeUnit: "pC", WaveformDigest: "c"},
	}
	index := 0
	findings, err := e.Assess("c", measurements, func() string { index++; return string(rune('0' + index)) }, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 3 || findings[0].Severity != domain.SeverityCritical {
		t.Fatalf("评估等级错误: %#v", findings)
	}
	want := []string{"PHASE_CONCENTRATION", "PEAK_CRITICAL", "RATE_CRITICAL"}
	for i, code := range want {
		if findings[0].RuleCodes[i] != code {
			t.Fatalf("规则排序错误: %#v", findings[0].RuleCodes)
		}
	}
}

func TestRetestClosesOnlyWithSufficientImprovement(t *testing.T) {
	e := MustDefault()
	original := domain.MeasurementBatch{ID: "a", CampaignID: "c", SensorPoint: "A", Phase: "A", PeakPCC: 150, RepetitionRate: 1200, NoiseFloor: 5, ApparentChargeUnit: "pC", WaveformDigest: "a"}
	retest := domain.MeasurementBatch{ID: "r", CampaignID: "c", SensorPoint: "A2", Phase: "A", PeakPCC: 30, RepetitionRate: 100, NoiseFloor: 5, ApparentChargeUnit: "pC", WaveformDigest: "r", IsRetest: true, SupersedesID: "a", SensorModel: "HFCT-B"}
	comparison, err := e.CompareRetest(original, retest)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Disposition != domain.DispositionClosed {
		t.Fatalf("改善充分应关闭问题: %#v", comparison)
	}
	retest.PeakPCC = 90
	comparison, err = e.CompareRetest(original, retest)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Disposition != domain.DispositionEscalated {
		t.Fatalf("峰值仍高应升级问题: %#v", comparison)
	}
}
