package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"pdreview/internal/domain"
)

var ErrIdempotencyMismatch = errors.New("幂等键已用于其他任务")

type LedgerEvent struct {
	Sequence       int64             `json:"sequence"`
	PreviousDigest string            `json:"previousDigest"`
	Digest         string            `json:"digest"`
	EventType      string            `json:"eventType"`
	IdempotencyKey string            `json:"idempotencyKey"`
	CampaignID     string            `json:"campaignID"`
	Actor          string            `json:"actor"`
	OccurredAt     time.Time         `json:"occurredAt"`
	Aggregate      *domain.Aggregate `json:"aggregate"`
}

type idempotencyRecord struct {
	CampaignID string            `json:"campaignID"`
	Sequence   int64             `json:"sequence"`
	Result     *domain.Aggregate `json:"result"`
}

type snapshot struct {
	SchemaVersion int                          `json:"schemaVersion"`
	LastSequence  int64                        `json:"lastSequence"`
	LastDigest    string                       `json:"lastDigest"`
	Campaigns     map[string]*domain.Aggregate `json:"campaigns"`
	Idempotency   map[string]idempotencyRecord `json:"idempotency"`
	WrittenAt     time.Time                    `json:"writtenAt"`
}

type Ledger struct {
	mu           sync.RWMutex
	directory    string
	ledgerPath   string
	snapshotPath string
	file         *os.File
	sequence     int64
	lastDigest   string
	campaigns    map[string]*domain.Aggregate
	idempotency  map[string]idempotencyRecord
}

func Open(directory string) (*Ledger, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, fmt.Errorf("数据目录不能为空")
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	l := &Ledger{
		directory:    directory,
		ledgerPath:   filepath.Join(directory, "events.jsonl"),
		snapshotPath: filepath.Join(directory, "snapshot.json"),
		campaigns:    make(map[string]*domain.Aggregate),
		idempotency:  make(map[string]idempotencyRecord),
	}
	if err := l.replayAndValidate(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(l.ledgerPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("打开事件账本: %w", err)
	}
	l.file = file
	return l, nil
}

func (l *Ledger) replayAndValidate() error {
	projectedCampaigns := make(map[string]*domain.Aggregate)
	projectedKeys := make(map[string]idempotencyRecord)
	file, err := os.Open(l.ledgerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, err := l.validateSnapshotAgainstProjection(projectedCampaigns, projectedKeys, nil, 0, "")
			return err
		}
		return fmt.Errorf("读取事件账本: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var sequence int64
	var previous string
	// digestBySeq 记录事件账本中每个序号对应的事件摘要，用于校验滞后的投影快照游标是否仍指向权威链上的真实事件。
	digestBySeq := []string{""}
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			return fmt.Errorf("事件账本第 %d 行为空", line)
		}
		var event LedgerEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("解析事件账本第 %d 行: %w", line, err)
		}
		if event.Sequence != sequence+1 {
			return fmt.Errorf("事件账本序号不连续: 得到 %d，期望 %d", event.Sequence, sequence+1)
		}
		if event.PreviousDigest != previous {
			return fmt.Errorf("事件账本第 %d 行前序摘要不匹配", line)
		}
		digest, err := digestEvent(event)
		if err != nil {
			return fmt.Errorf("计算事件摘要: %w", err)
		}
		if event.Digest != digest {
			return fmt.Errorf("事件账本第 %d 行摘要校验失败", line)
		}
		if event.Aggregate == nil || event.Aggregate.Campaign.ID != event.CampaignID {
			return fmt.Errorf("事件账本第 %d 行任务投影无效", line)
		}
		if event.IdempotencyKey == "" {
			return fmt.Errorf("事件账本第 %d 行缺少幂等键", line)
		}
		if old, exists := projectedKeys[event.IdempotencyKey]; exists && old.CampaignID != event.CampaignID {
			return fmt.Errorf("事件账本幂等键冲突: %s", event.IdempotencyKey)
		}
		clone, err := event.Aggregate.Clone()
		if err != nil {
			return fmt.Errorf("克隆任务投影: %w", err)
		}
		projectedCampaigns[event.CampaignID] = clone
		result, err := event.Aggregate.Clone()
		if err != nil {
			return fmt.Errorf("克隆幂等结果: %w", err)
		}
		projectedKeys[event.IdempotencyKey] = idempotencyRecord{CampaignID: event.CampaignID, Sequence: event.Sequence, Result: result}
		sequence = event.Sequence
		previous = event.Digest
		digestBySeq = append(digestBySeq, previous)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("扫描事件账本: %w", err)
	}
	healed, err := l.validateSnapshotAgainstProjection(projectedCampaigns, projectedKeys, digestBySeq, sequence, previous)
	if err != nil {
		return err
	}
	l.campaigns = projectedCampaigns
	l.idempotency = projectedKeys
	l.sequence = sequence
	l.lastDigest = previous
	// 当启动检测到投影快照滞后于权威事件账本（崩溃发生在事件日志 fsync 之后、新快照原子替换之前）时，
	// 以权威账本投影自愈一次投影快照，使后续启动回到快照与日志对齐的稳态。
	if healed {
		if err := l.writeSnapshotLocked(time.Now()); err != nil {
			return fmt.Errorf("自愈投影快照: %w", err)
		}
	}
	return nil
}

// validateSnapshotAgainstProjection 校验磁盘上的投影快照是否与以事件账本权威重放得到的投影一致。
// digestBySeq 为长度 sequence+1 的切片，digestBySeq[0]==""，digestBySeq[i] 为第 i 条事件摘要；
// 据此可判断滞后快照游标 (LastSequence, LastDigest) 是否仍指向权威链上的真实事件。
// 返回 healed=true 表示快照曾落后于权威账本、已按账本投影恢复，调用方据此触发一次自愈写入。
func (l *Ledger) validateSnapshotAgainstProjection(campaigns map[string]*domain.Aggregate, keys map[string]idempotencyRecord, digestBySeq []string, sequence int64, digest string) (bool, error) {
	b, err := os.ReadFile(l.snapshotPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			l.campaigns, l.idempotency, l.sequence, l.lastDigest = campaigns, keys, sequence, digest
			return false, nil
		}
		return false, fmt.Errorf("读取快照: %w", err)
	}
	var saved snapshot
	if err := json.Unmarshal(b, &saved); err != nil {
		return false, fmt.Errorf("解析快照: %w", err)
	}
	if saved.SchemaVersion != 1 {
		return false, fmt.Errorf("快照模式版本不一致")
	}
	// 快照游标恰好对齐权威账本末端时仍保持严格一致性校验，以保留篡改检测行为。
	if saved.LastSequence == sequence {
		if saved.LastDigest != digest {
			return false, fmt.Errorf("快照与事件账本游标不一致")
		}
		if len(saved.Campaigns) != len(campaigns) || len(saved.Idempotency) != len(keys) {
			return false, fmt.Errorf("快照与事件账本投影数量不一致")
		}
		for id, replayed := range campaigns {
			snapshotCampaign, ok := saved.Campaigns[id]
			if !ok || !reflect.DeepEqual(snapshotCampaign, replayed) {
				return false, fmt.Errorf("快照任务 %s 的版本不一致", id)
			}
		}
		if !reflect.DeepEqual(saved.Idempotency, keys) {
			return false, fmt.Errorf("快照幂等投影与事件账本不一致")
		}
		l.campaigns, l.idempotency, l.sequence, l.lastDigest = campaigns, keys, sequence, digest
		return false, nil
	}
	// 快照游标超前于权威账本（账本缺失尾部事件）视为不可恢复的损坏。
	if saved.LastSequence > sequence {
		return false, fmt.Errorf("快照游标超前于事件账本")
	}
	// 快照游标落在权威链内部：仅当其 LastDigest 与该序号对应的事件摘要一致时，
	// 才认定快照是崩溃恢复中残留的滞后投影，按 replayRecoveryPolicy 以账本投影自愈。
	if int(saved.LastSequence) >= len(digestBySeq) {
		return false, fmt.Errorf("快照游标超出事件账本序号范围")
	}
	if saved.LastDigest != digestBySeq[saved.LastSequence] {
		return false, fmt.Errorf("快照游标与事件账本链不一致")
	}
	l.campaigns, l.idempotency, l.sequence, l.lastDigest = campaigns, keys, sequence, digest
	return true, nil
}

func digestEvent(event LedgerEvent) (string, error) {
	payload := struct {
		Sequence       int64             `json:"sequence"`
		PreviousDigest string            `json:"previousDigest"`
		EventType      string            `json:"eventType"`
		IdempotencyKey string            `json:"idempotencyKey"`
		CampaignID     string            `json:"campaignID"`
		Actor          string            `json:"actor"`
		OccurredAt     time.Time         `json:"occurredAt"`
		Aggregate      *domain.Aggregate `json:"aggregate"`
	}{
		Sequence: event.Sequence, PreviousDigest: event.PreviousDigest, EventType: event.EventType,
		IdempotencyKey: event.IdempotencyKey, CampaignID: event.CampaignID, Actor: event.Actor,
		OccurredAt: event.OccurredAt.UTC(), Aggregate: event.Aggregate,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (l *Ledger) Save(aggregate *domain.Aggregate, expectedPreviousVersion int64, idempotencyKey, eventType, actor string, now time.Time) (*domain.Aggregate, bool, error) {
	if aggregate == nil || aggregate.Campaign.ID == "" {
		return nil, false, fmt.Errorf("%w: 待保存任务为空", domain.ErrInvalid)
	}
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 160 {
		return nil, false, fmt.Errorf("%w: Idempotency-Key 为空或过长", domain.ErrInvalid)
	}
	if strings.TrimSpace(eventType) == "" || strings.TrimSpace(actor) == "" {
		return nil, false, fmt.Errorf("%w: 事件类型和操作者不能为空", domain.ErrInvalid)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if record, exists := l.idempotency[idempotencyKey]; exists {
		if record.CampaignID != aggregate.Campaign.ID {
			return nil, false, ErrIdempotencyMismatch
		}
		stored := l.campaigns[record.CampaignID]
		clone, err := stored.Clone()
		return clone, true, err
	}
	current, exists := l.campaigns[aggregate.Campaign.ID]
	if !exists {
		if expectedPreviousVersion != 0 || aggregate.Campaign.Version != 1 {
			return nil, false, domain.ErrVersion
		}
	} else {
		if current.Campaign.Version != expectedPreviousVersion || aggregate.Campaign.Version != expectedPreviousVersion+1 {
			return nil, false, domain.ErrVersion
		}
	}
	clone, err := aggregate.Clone()
	if err != nil {
		return nil, false, fmt.Errorf("克隆待保存任务: %w", err)
	}
	event := LedgerEvent{
		Sequence: l.sequence + 1, PreviousDigest: l.lastDigest, EventType: eventType,
		IdempotencyKey: idempotencyKey, CampaignID: aggregate.Campaign.ID,
		Actor: strings.TrimSpace(actor), OccurredAt: now.UTC(), Aggregate: clone,
	}
	event.Digest, err = digestEvent(event)
	if err != nil {
		return nil, false, fmt.Errorf("生成事件摘要: %w", err)
	}
	line, err := json.Marshal(event)
	if err != nil {
		return nil, false, fmt.Errorf("编码事件: %w", err)
	}
	if _, err := l.file.Write(append(line, '\n')); err != nil {
		return nil, false, fmt.Errorf("追加事件账本: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return nil, false, fmt.Errorf("同步事件账本: %w", err)
	}
	l.sequence = event.Sequence
	l.lastDigest = event.Digest
	l.campaigns[aggregate.Campaign.ID] = clone
	idempotentResult, err := clone.Clone()
	if err != nil {
		return nil, false, err
	}
	l.idempotency[idempotencyKey] = idempotencyRecord{CampaignID: aggregate.Campaign.ID, Sequence: event.Sequence, Result: idempotentResult}
	if err := l.writeSnapshotLocked(now); err != nil {
		return nil, false, err
	}
	result, err := clone.Clone()
	return result, false, err
}

func (l *Ledger) writeSnapshotLocked(now time.Time) error {
	state := snapshot{
		SchemaVersion: 1, LastSequence: l.sequence, LastDigest: l.lastDigest,
		Campaigns: l.campaigns, Idempotency: l.idempotency, WrittenAt: now.UTC(),
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("编码投影快照: %w", err)
	}
	temp, err := os.CreateTemp(l.directory, "snapshot-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时快照: %w", err)
	}
	tempName := temp.Name()
	cleanup := func() { _ = os.Remove(tempName) }
	if err := temp.Chmod(0o640); err != nil {
		_ = temp.Close()
		cleanup()
		return fmt.Errorf("设置快照权限: %w", err)
	}
	if _, err := temp.Write(b); err != nil {
		_ = temp.Close()
		cleanup()
		return fmt.Errorf("写入临时快照: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		cleanup()
		return fmt.Errorf("同步临时快照: %w", err)
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭临时快照: %w", err)
	}
	if err := os.Rename(tempName, l.snapshotPath); err != nil {
		cleanup()
		return fmt.Errorf("原子替换快照: %w", err)
	}
	dir, err := os.Open(l.directory)
	if err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (l *Ledger) GetCampaign(id string) (*domain.Aggregate, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	aggregate, ok := l.campaigns[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return aggregate.Clone()
}

func (l *Ledger) GetIdempotent(key string) (*domain.Aggregate, bool, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	record, ok := l.idempotency[strings.TrimSpace(key)]
	if !ok {
		return nil, false, nil
	}
	if record.Result == nil {
		return nil, false, fmt.Errorf("幂等记录关联任务不存在")
	}
	clone, err := record.Result.Clone()
	return clone, true, err
}

func (l *Ledger) ListCampaigns() ([]*domain.Aggregate, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	result := make([]*domain.Aggregate, 0, len(l.campaigns))
	for _, aggregate := range l.campaigns {
		clone, err := aggregate.Clone()
		if err != nil {
			return nil, err
		}
		result = append(result, clone)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Campaign.UpdatedAt.Equal(result[j].Campaign.UpdatedAt) {
			return result[i].Campaign.ID < result[j].Campaign.ID
		}
		return result[i].Campaign.UpdatedAt.After(result[j].Campaign.UpdatedAt)
	})
	return result, nil
}

func (l *Ledger) FindCredential(code string) (*domain.ReleaseCredential, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	code = strings.TrimSpace(code)
	for _, aggregate := range l.campaigns {
		if aggregate.Credential != nil && aggregate.Credential.CredentialCode == code {
			credential := *aggregate.Credential
			return &credential, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (l *Ledger) VerifyCredential(code string) (*domain.ReleaseCredential, bool, string, error) {
	credential, err := l.FindCredential(code)
	if err != nil {
		return nil, false, "未找到该凭据", err
	}
	aggregate, err := l.GetCampaign(credential.CampaignID)
	if err != nil {
		return credential, false, "凭据关联任务不存在", err
	}
	if credential.RevokedAt != nil {
		return credential, false, "凭据已撤销", nil
	}
	if aggregate.Credential == nil || aggregate.Campaign.Status != domain.StatusReleased {
		return credential, false, "任务未处于已放行状态", nil
	}
	if credential.EvidenceDigest != aggregate.EvidenceDigest || credential.CampaignVersion != aggregate.FrozenVersion {
		return credential, false, "凭据与冻结证据不一致", nil
	}
	return credential, true, "凭据有效，校验码与冻结证据一致", nil
}

func (l *Ledger) Flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("刷新事件账本: %w", err)
	}
	return l.writeSnapshotLocked(time.Now())
}

func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	var result error
	if err := l.file.Sync(); err != nil {
		result = err
	}
	if err := l.file.Close(); err != nil && result == nil {
		result = err
	}
	l.file = nil
	return result
}

func ReadEvents(reader io.Reader) ([]LedgerEvent, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var result []LedgerEvent
	for scanner.Scan() {
		var event LedgerEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, scanner.Err()
}
