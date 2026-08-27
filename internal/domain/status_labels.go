package domain

// StatusLabel 返回适合浏览器展示的状态文案。
func StatusLabel(status CampaignStatus) string {
	switch status {
	case StatusDraft:
		return "草稿"
	case StatusAwaitingCollection:
		return "待采集"
	case StatusCollected:
		return "已采集"
	case StatusAwaitingRetest:
		return "待复验"
	case StatusAwaitingReview:
		return "待审核"
	case StatusApproved:
		return "已审核"
	case StatusFrozen:
		return "已冻结"
	case StatusReleased:
		return "已放行"
	default:
		return string(status)
	}
}
