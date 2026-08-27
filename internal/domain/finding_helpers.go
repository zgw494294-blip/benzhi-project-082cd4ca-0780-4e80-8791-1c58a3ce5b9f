package domain

// FindingIsOpen 判断发现是否仍需整改或复验。
func FindingIsOpen(finding AssessmentFinding) bool {
	return finding.Disposition == DispositionOpen
}
