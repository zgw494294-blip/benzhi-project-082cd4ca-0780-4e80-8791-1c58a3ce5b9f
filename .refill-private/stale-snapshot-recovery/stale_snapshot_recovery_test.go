package stalesnapshotrecovery_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pdreview/internal/domain"
	"pdreview/internal/store"
)

func TestRestartRecoversAndHealsSnapshotBehindLedger(t *testing.T) {
	directory := t.TempDir()
	ledger, err := store.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	aggregate, err := domain.NewCampaign("cmp-snapshot", domain.CampaignDraft{
		CableSegment: "快照恢复测试段", InsulationStructure: "XLPE", RatedVoltageKV: 220,
		TestPlan: "分级升压", Operator: "试验员",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, _, err = ledger.Save(aggregate, 0, "create-snapshot", "campaign.created", "试验员", now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(directory, "snapshot.json")
	staleSnapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	measurements := []domain.MeasurementBatch{
		{ID: "m-a", SensorPoint: "A 相", Phase: "A", PeakPCC: 20, RepetitionRate: 100, NoiseFloor: 2, ApparentChargeUnit: "pC", WaveformDigest: "a"},
		{ID: "m-b", SensorPoint: "B 相", Phase: "B", PeakPCC: 21, RepetitionRate: 110, NoiseFloor: 2, ApparentChargeUnit: "pC", WaveformDigest: "b"},
		{ID: "m-c", SensorPoint: "C 相", Phase: "C", PeakPCC: 19, RepetitionRate: 90, NoiseFloor: 2, ApparentChargeUnit: "pC", WaveformDigest: "c"},
	}
	if err := aggregate.AddInitialMeasurements(measurements, "试验员", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Save(aggregate, 1, "capture-snapshot", "measurements.captured", "试验员", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	// 模拟事件日志已经 fsync、但新快照尚未完成原子替换时进程退出。
	if err := os.WriteFile(snapshotPath, staleSnapshot, 0o640); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(directory)
	if err != nil {
		t.Fatalf("有效账本领先旧快照时重启失败: %v", err)
	}
	defer reopened.Close()
	recovered, err := reopened.GetCampaign(aggregate.Campaign.ID)
	if err != nil {
		t.Fatalf("未从有效账本恢复最新任务投影: %v", err)
	}
	if recovered.Campaign.Version != 2 {
		t.Fatalf("未从有效账本恢复最新任务投影: version=%d", recovered.Campaign.Version)
	}
	repairedBytes, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	var repaired struct {
		LastSequence int64 `json:"lastSequence"`
	}
	if err := json.Unmarshal(repairedBytes, &repaired); err != nil {
		t.Fatal(err)
	}
	if repaired.LastSequence != 2 {
		t.Fatalf("启动恢复后未自愈落后的快照: lastSequence=%d", repaired.LastSequence)
	}
}
