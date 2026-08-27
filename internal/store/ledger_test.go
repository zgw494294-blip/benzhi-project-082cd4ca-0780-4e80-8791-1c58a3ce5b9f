package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pdreview/internal/domain"
)

func TestLedgerReplayIdempotencyAndTamperDetection(t *testing.T) {
	directory := t.TempDir()
	ledger, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	agg, err := domain.NewCampaign("cmp-store", domain.CampaignDraft{CableSegment: "电缆段", InsulationStructure: "XLPE", RatedVoltageKV: 110, TestPlan: "测试方案", Operator: "试验员"}, now)
	if err != nil {
		t.Fatal(err)
	}
	stored, replayed, err := ledger.Save(agg, 0, "create-key", "campaign.created", "试验员", now)
	if err != nil || replayed || stored.Campaign.Version != 1 {
		t.Fatalf("首次保存失败: %#v %v", stored, err)
	}
	result, replayed, err := ledger.Save(agg, 0, "create-key", "campaign.created", "试验员", now)
	if err != nil || !replayed || result.Campaign.Version != 1 {
		t.Fatalf("幂等重放失败: %#v %v", result, err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory)
	if err != nil {
		t.Fatalf("合法账本无法重放: %v", err)
	}
	items, err := reopened.ListCampaigns()
	if err != nil || len(items) != 1 {
		t.Fatalf("重放投影错误: %#v %v", items, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(directory, "events.jsonl")
	b, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 1
	if err := os.WriteFile(ledgerPath, b, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(directory); err == nil {
		t.Fatal("被篡改的事件账本不应通过启动校验")
	}
}
