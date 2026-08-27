package assessment

import "pdreview/internal/domain"

// SeverityRank 提供稳定的风险等级排序值。
func SeverityRank(severity domain.Severity) int {
	switch severity {
	case domain.SeverityCritical:
		return 4
	case domain.SeverityWatch:
		return 3
	case domain.SeverityNormal:
		return 1
	default:
		return 0
	}
}
