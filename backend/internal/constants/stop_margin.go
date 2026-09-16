package constants

const (
	// 安全停机余量台账状态：有效结论 / 待复核（任一来源缺失时不结算，仅登记）
	LedgerStatusValid         = "valid"
	LedgerStatusPendingReview = "pending_review"

	// 台账结论：余量充足 / 余量不足；待复核台账结论为空
	LedgerConclusionSufficient   = "sufficient"
	LedgerConclusionInsufficient = "insufficient"
)
