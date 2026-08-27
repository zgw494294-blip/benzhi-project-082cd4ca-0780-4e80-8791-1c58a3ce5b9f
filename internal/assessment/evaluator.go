package assessment

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"pdreview/internal/domain"
)

type Thresholds struct {
	WatchPeakPCC       float64
	CriticalPeakPCC    float64
	WatchRepetition    float64
	CriticalRepetition float64
	MinimumSignalRatio float64
	PhaseDominance     float64
	RetestDropRatio    float64
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		WatchPeakPCC: 50, CriticalPeakPCC: 100,
		WatchRepetition: 300, CriticalRepetition: 1000,
		MinimumSignalRatio: 2, PhaseDominance: 3, RetestDropRatio: 0.4,
	}
}

type Evaluator struct {
	thresholds Thresholds
}

type RuleHit struct {
	Code           string
	Severity       domain.Severity
	Explanation    string
	Recommendation string
	Priority       int
}

type RetestComparison struct {
	Disposition domain.FindingDisposition `json:"disposition"`
	Explanation string                    `json:"explanation"`
	PeakChange  float64                   `json:"peakChange"`
	RateChange  float64                   `json:"rateChange"`
}

func New(thresholds Thresholds) (*Evaluator, error) {
	if thresholds.WatchPeakPCC <= 0 || thresholds.CriticalPeakPCC <= thresholds.WatchPeakPCC {
		return nil, fmt.Errorf("峰值阈值配置无效")
	}
	if thresholds.WatchRepetition <= 0 || thresholds.CriticalRepetition <= thresholds.WatchRepetition {
		return nil, fmt.Errorf("重复率阈值配置无效")
	}
	if thresholds.MinimumSignalRatio <= 1 || thresholds.PhaseDominance <= 1 {
		return nil, fmt.Errorf("比值阈值配置无效")
	}
	if thresholds.RetestDropRatio <= 0 || thresholds.RetestDropRatio >= 1 {
		return nil, fmt.Errorf("复验下降比例配置无效")
	}
	return &Evaluator{thresholds: thresholds}, nil
}

func MustDefault() *Evaluator {
	e, err := New(DefaultThresholds())
	if err != nil {
		panic(err)
	}
	return e
}

func (e *Evaluator) Assess(campaignID string, measurements []domain.MeasurementBatch, idFactory func() string, now time.Time) ([]domain.AssessmentFinding, error) {
	if strings.TrimSpace(campaignID) == "" || idFactory == nil {
		return nil, fmt.Errorf("评估输入不完整")
	}
	initial := make([]domain.MeasurementBatch, 0, len(measurements))
	for _, m := range measurements {
		if !m.IsRetest {
			if err := m.Validate(); err != nil {
				return nil, err
			}
			initial = append(initial, m)
		}
	}
	if len(initial) != 3 {
		return nil, fmt.Errorf("评估需要三相首次测量")
	}
	sort.Slice(initial, func(i, j int) bool {
		if initial[i].Phase == initial[j].Phase {
			return initial[i].SensorPoint < initial[j].SensorPoint
		}
		return initial[i].Phase < initial[j].Phase
	})
	dominantID, dominance := phaseDominance(initial)
	findings := make([]domain.AssessmentFinding, 0, len(initial))
	for _, m := range initial {
		hits := e.evaluateMeasurement(m)
		if m.ID == dominantID && dominance >= e.thresholds.PhaseDominance {
			hits = append(hits, RuleHit{
				Code: "PHASE_CONCENTRATION", Severity: domain.SeverityCritical, Priority: 10,
				Explanation:    fmt.Sprintf("%s 相峰值相对其他相均值达到 %.2f 倍", m.Phase, dominance),
				Recommendation: "核查该相终端、电缆接头与传感器耦合，并安排同相替代测点复验",
			})
		}
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].Priority == hits[j].Priority {
				return hits[i].Code < hits[j].Code
			}
			return hits[i].Priority < hits[j].Priority
		})
		severity := domain.SeverityNormal
		codes := make([]string, 0, len(hits))
		explanations := make([]string, 0, len(hits))
		recommendations := make([]string, 0, len(hits))
		for _, hit := range hits {
			severity = maxSeverity(severity, hit.Severity)
			codes = append(codes, hit.Code)
			explanations = append(explanations, hit.Explanation)
			recommendations = appendUnique(recommendations, hit.Recommendation)
		}
		if len(hits) == 0 {
			codes = []string{"WITHIN_LIMITS"}
			explanations = []string{"峰值、重复率、噪声比和相位分布均在规则限值内"}
			recommendations = []string{"保持当前试验接线与采集参数"}
		}
		finding := domain.AssessmentFinding{
			ID: idFactory(), CampaignID: campaignID, MeasurementID: m.ID,
			Severity: severity, RuleCodes: codes, Explanation: strings.Join(explanations, "；"),
			Recommendation: strings.Join(recommendations, "；"), CreatedAt: now.UTC(),
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

func (e *Evaluator) evaluateMeasurement(m domain.MeasurementBatch) []RuleHit {
	var hits []RuleHit
	if m.PeakPCC >= e.thresholds.CriticalPeakPCC {
		hits = append(hits, RuleHit{
			Code: "PEAK_CRITICAL", Severity: domain.SeverityCritical, Priority: 20,
			Explanation:    fmt.Sprintf("峰值 %.2f pC 达到严重阈值 %.2f pC", m.PeakPCC, e.thresholds.CriticalPeakPCC),
			Recommendation: "检查绝缘缺陷位置并使用替代传感器复验",
		})
	} else if m.PeakPCC >= e.thresholds.WatchPeakPCC {
		hits = append(hits, RuleHit{
			Code: "PEAK_WATCH", Severity: domain.SeverityWatch, Priority: 21,
			Explanation:    fmt.Sprintf("峰值 %.2f pC 达到关注阈值 %.2f pC", m.PeakPCC, e.thresholds.WatchPeakPCC),
			Recommendation: "复核校准脉冲并安排同条件复测",
		})
	}
	if m.RepetitionRate >= e.thresholds.CriticalRepetition {
		hits = append(hits, RuleHit{
			Code: "RATE_CRITICAL", Severity: domain.SeverityCritical, Priority: 30,
			Explanation:    fmt.Sprintf("重复率 %.2f 次/秒达到严重阈值 %.2f", m.RepetitionRate, e.thresholds.CriticalRepetition),
			Recommendation: "排除连续放电源并延长采样窗口复验",
		})
	} else if m.RepetitionRate >= e.thresholds.WatchRepetition {
		hits = append(hits, RuleHit{
			Code: "RATE_WATCH", Severity: domain.SeverityWatch, Priority: 31,
			Explanation:    fmt.Sprintf("重复率 %.2f 次/秒达到关注阈值 %.2f", m.RepetitionRate, e.thresholds.WatchRepetition),
			Recommendation: "复核试验电源与接地回路后复测",
		})
	}
	denominator := math.Max(m.NoiseFloor, 0.1)
	ratio := m.PeakPCC / denominator
	if ratio < e.thresholds.MinimumSignalRatio {
		hits = append(hits, RuleHit{
			Code: "LOW_SIGNAL_RATIO", Severity: domain.SeverityWatch, Priority: 40,
			Explanation:    fmt.Sprintf("信噪比 %.2f 低于最小值 %.2f", ratio, e.thresholds.MinimumSignalRatio),
			Recommendation: "降低现场干扰或更换耦合传感器后复测",
		})
	}
	return hits
}

func phaseDominance(items []domain.MeasurementBatch) (string, float64) {
	if len(items) < 2 {
		return "", 0
	}
	maxIndex := 0
	for i := 1; i < len(items); i++ {
		if items[i].PeakPCC > items[maxIndex].PeakPCC {
			maxIndex = i
		}
	}
	otherTotal := 0.0
	for i := range items {
		if i != maxIndex {
			otherTotal += items[i].PeakPCC
		}
	}
	otherAverage := otherTotal / float64(len(items)-1)
	if otherAverage <= 0 {
		if items[maxIndex].PeakPCC > 0 {
			return items[maxIndex].ID, math.Inf(1)
		}
		return items[maxIndex].ID, 1
	}
	return items[maxIndex].ID, items[maxIndex].PeakPCC / otherAverage
}

func (e *Evaluator) CompareRetest(original, retest domain.MeasurementBatch) (RetestComparison, error) {
	if original.IsRetest || !retest.IsRetest || retest.SupersedesID != original.ID {
		return RetestComparison{}, fmt.Errorf("复验关联关系无效")
	}
	if err := original.Validate(); err != nil {
		return RetestComparison{}, err
	}
	if err := retest.Validate(); err != nil {
		return RetestComparison{}, err
	}
	peakChange := relativeChange(original.PeakPCC, retest.PeakPCC)
	rateChange := relativeChange(original.RepetitionRate, retest.RepetitionRate)
	ratio := retest.PeakPCC / math.Max(retest.NoiseFloor, 0.1)
	peakSafe := retest.PeakPCC < e.thresholds.WatchPeakPCC
	rateSafe := retest.RepetitionRate < e.thresholds.WatchRepetition
	dropped := peakChange <= -e.thresholds.RetestDropRatio
	if peakSafe && rateSafe && ratio >= e.thresholds.MinimumSignalRatio && dropped {
		return RetestComparison{
			Disposition: domain.DispositionClosed, PeakChange: peakChange, RateChange: rateChange,
			Explanation: fmt.Sprintf("复验峰值下降 %.1f%%，且峰值、重复率与信噪比满足关闭条件", -peakChange*100),
		}, nil
	}
	reasons := make([]string, 0, 4)
	if !peakSafe {
		reasons = append(reasons, "峰值仍高于关注阈值")
	}
	if !rateSafe {
		reasons = append(reasons, "重复率仍高于关注阈值")
	}
	if ratio < e.thresholds.MinimumSignalRatio {
		reasons = append(reasons, "信噪比仍不足")
	}
	if !dropped {
		reasons = append(reasons, "峰值下降幅度不足")
	}
	return RetestComparison{
		Disposition: domain.DispositionEscalated, PeakChange: peakChange, RateChange: rateChange,
		Explanation: "问题升级：" + strings.Join(reasons, "、"),
	}, nil
}

func relativeChange(before, after float64) float64 {
	if before == 0 {
		if after == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return (after - before) / before
}

func maxSeverity(a, b domain.Severity) domain.Severity {
	rank := map[domain.Severity]int{domain.SeverityNormal: 0, domain.SeverityWatch: 1, domain.SeverityCritical: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
