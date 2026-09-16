package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"robot-cell-safety-envelope-validator/backend/internal/constants"
	"robot-cell-safety-envelope-validator/backend/internal/dto"
	"robot-cell-safety-envelope-validator/backend/internal/geometry"
	"robot-cell-safety-envelope-validator/backend/internal/model"
	"robot-cell-safety-envelope-validator/backend/internal/repository"
)

// StopMarginLedgerService 安全停机余量台账。
//
// 结算模型：停机距离 S = v·t + v²/(2a)，余量 M = 路径最小间距 − S。
//
//	v —— 运动程序轨迹最高速度 (mm/s)，结算时从程序版本快照提取；
//	t —— 控制器响应时间 (ms)，多来源取最保守（最大）值；
//	a —— 制动减速度 (mm/s²)，多来源取最保守（最小）值。
//
// 台账规则：
//  1. 任一已声明来源缺失或非法 → 状态 pending_review，不结算距离与余量；
//  2. pending_review 台账不得覆盖程序已有的有效结论（不推进当前结论指针）；
//  3. 同一输入（规范化快照哈希相同）并发或重复提交只生成一条台账；
//  4. 台账仅追加，程序或设备参数更新产生新台账，历史记录只读。
//
// Settle 是本模块唯一的执行入口。
type StopMarginLedgerService struct {
	db         *gorm.DB
	repository *repository.StopMarginLedgerRepository
	programs   *repository.MotionProgramRepository
	system     *SystemService
}

func NewStopMarginLedgerService(db *gorm.DB, repository *repository.StopMarginLedgerRepository, programs *repository.MotionProgramRepository, system *SystemService) *StopMarginLedgerService {
	return &StopMarginLedgerService{db: db, repository: repository, programs: programs, system: system}
}

type stopMarginSnapshotSource struct {
	Source string   `json:"source"`
	Value  *float64 `json:"value"`
}

// stopMarginSnapshot 结算输入的规范化快照：来源已排序、数值已归一化，哈希与结算共用同一份数据。
type stopMarginSnapshot struct {
	MotionProgramID     uint                       `json:"motion_program_id"`
	ProgramCode         string                     `json:"program_code"`
	ProgramVersion      int                        `json:"program_version"`
	SourceChecksum      string                     `json:"source_checksum"`
	MaxSpeedMMS         float64                    `json:"max_speed_mm_s"`
	MinSeparationMM     float64                    `json:"min_separation_mm"`
	ResponseTimeSources []stopMarginSnapshotSource `json:"response_time_sources"`
	DecelerationSources []stopMarginSnapshotSource `json:"deceleration_sources"`
}

type stopMarginResolution struct {
	responseTimeMS   *float64
	decelerationMMS2 *float64
	missing          []string
}

func (service *StopMarginLedgerService) Settle(request dto.SettleStopMarginRequest, actor dto.Actor, requestID string) (dto.StopMarginLedgerResponse, bool, error) {
	program, err := service.programs.Get(request.MotionProgramID)
	if err != nil {
		return dto.StopMarginLedgerResponse{}, false, MapRepositoryError("motion program", err)
	}
	if program.ProgramState != constants.ProgramStateReady && program.ProgramState != constants.ProgramStateActive {
		return dto.StopMarginLedgerResponse{}, false, Conflict("program_not_ready", "only ready or active programs can be settled", repository.ErrStateConflict)
	}
	maxSpeed, err := programMaxSpeed(program)
	if err != nil {
		return dto.StopMarginLedgerResponse{}, false, Unprocessable("invalid_program_trajectory", err.Error(), err)
	}
	snapshot, err := buildStopMarginSnapshot(program, maxSpeed, request)
	if err != nil {
		return dto.StopMarginLedgerResponse{}, false, BadRequest("invalid_source_definition", err.Error())
	}
	snapshotJSON, _ := json.Marshal(snapshot)
	inputHash := stopMarginInputHash(snapshotJSON)
	if existing, err := service.repository.FindByInputHash(inputHash); err == nil {
		response, responseErr := service.decorateOne(existing)
		response.Reused = true
		return response, true, responseErr
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return dto.StopMarginLedgerResponse{}, false, Internal("could not check prior ledger input", err)
	}
	resolution := resolveStopMarginSources(snapshot)
	entry := model.StopMarginLedger{
		MotionProgramID:    program.ID,
		InputHash:          inputHash,
		InputSnapshotJSON:  string(snapshotJSON),
		MaxSpeedMMS:        maxSpeed,
		MinSeparationMM:    request.MinSeparationMM,
		MissingSourcesJSON: string(mustMarshalJSON(resolution.missing)),
		LedgerStatus:       constants.LedgerStatusValid,
		SettledBy:          actor.ID,
		SettledAt:          time.Now().UTC(),
	}
	switch {
	case len(resolution.missing) > 0:
		entry.LedgerStatus = constants.LedgerStatusPendingReview
	default:
		responseSeconds := *resolution.responseTimeMS / 1000
		stopping := maxSpeed*responseSeconds + maxSpeed*maxSpeed/(2**resolution.decelerationMMS2)
		margin := request.MinSeparationMM - stopping
		if !stopMarginFinite(stopping) || !stopMarginFinite(margin) {
			entry.LedgerStatus = constants.LedgerStatusPendingReview
			entry.MissingSourcesJSON = string(mustMarshalJSON([]string{"settlement:non_finite_result"}))
			break
		}
		entry.ResponseTimeMS = resolution.responseTimeMS
		entry.DecelerationMMS2 = resolution.decelerationMMS2
		entry.StoppingDistanceMM = &stopping
		entry.MarginMM = &margin
		entry.Conclusion = constants.LedgerConclusionSufficient
		if margin < 0 {
			entry.Conclusion = constants.LedgerConclusionInsufficient
		}
	}
	err = service.db.Transaction(func(tx *gorm.DB) error {
		repo := service.repository.WithDB(tx)
		if err := repo.Create(&entry); err != nil {
			return err
		}
		if entry.LedgerStatus == constants.LedgerStatusValid {
			current := model.StopMarginCurrent{MotionProgramID: program.ID, LedgerID: entry.ID, UpdatedAt: entry.SettledAt}
			if err := repo.AdvanceCurrent(&current); err != nil {
				return err
			}
		}
		return service.system.RecordAuditTx(tx, actor, requestID, "stop_margin_ledger.settled", "stop_margin_ledger", auditID(entry.ID), map[string]any{
			"input_hash": inputHash, "motion_program_id": program.ID, "program_version": program.Version,
		}, nil, map[string]any{
			"ledger_status": entry.LedgerStatus, "conclusion": entry.Conclusion, "margin_mm": entry.MarginMM, "missing_sources": resolution.missing,
		})
	})
	if err != nil {
		if repository.IsUniqueViolation(err) {
			if existing, lookupErr := service.repository.FindByInputHash(inputHash); lookupErr == nil {
				response, responseErr := service.decorateOne(existing)
				response.Reused = true
				return response, true, responseErr
			}
			return dto.StopMarginLedgerResponse{}, false, Conflict("ledger_input_conflict", "an identical settlement was recorded concurrently", err)
		}
		return dto.StopMarginLedgerResponse{}, false, Internal("could not persist stop margin ledger", err)
	}
	response, err := service.decorateOne(entry)
	return response, false, err
}

func (service *StopMarginLedgerService) Get(id uint) (dto.StopMarginLedgerResponse, error) {
	entry, err := service.repository.Get(id)
	if err != nil {
		return dto.StopMarginLedgerResponse{}, MapRepositoryError("stop margin ledger", err)
	}
	return service.decorateOne(entry)
}

func (service *StopMarginLedgerService) List(page, pageSize int, programID uint, status string) ([]dto.StopMarginLedgerResponse, dto.PageMeta, error) {
	entries, total, err := service.repository.List(page, pageSize, programID, status)
	if err != nil {
		return nil, dto.PageMeta{}, Internal("could not list stop margin ledgers", err)
	}
	responses, err := service.decorate(entries)
	if err != nil {
		return nil, dto.PageMeta{}, err
	}
	return responses, PageMeta(page, pageSize, total), nil
}

func (service *StopMarginLedgerService) decorateOne(entry model.StopMarginLedger) (dto.StopMarginLedgerResponse, error) {
	responses, err := service.decorate([]model.StopMarginLedger{entry})
	if err != nil {
		return dto.StopMarginLedgerResponse{}, err
	}
	return responses[0], nil
}

func (service *StopMarginLedgerService) decorate(entries []model.StopMarginLedger) ([]dto.StopMarginLedgerResponse, error) {
	programIDs := make([]uint, 0, len(entries))
	seenPrograms := map[uint]bool{}
	for _, entry := range entries {
		if !seenPrograms[entry.MotionProgramID] {
			seenPrograms[entry.MotionProgramID] = true
			programIDs = append(programIDs, entry.MotionProgramID)
		}
	}
	currents, err := service.repository.CurrentForPrograms(programIDs)
	if err != nil {
		return nil, Internal("could not load current conclusions", err)
	}
	ledgerIDs := make([]uint, 0, len(currents))
	seenLedgers := map[uint]bool{}
	for _, current := range currents {
		if !seenLedgers[current.LedgerID] {
			seenLedgers[current.LedgerID] = true
			ledgerIDs = append(ledgerIDs, current.LedgerID)
		}
	}
	currentLedgers, err := service.repository.LedgersByIDs(ledgerIDs)
	if err != nil {
		return nil, Internal("could not load current ledger entries", err)
	}
	responses := make([]dto.StopMarginLedgerResponse, 0, len(entries))
	for _, entry := range entries {
		responses = append(responses, stopMarginResponse(entry, currents, currentLedgers))
	}
	return responses, nil
}

func stopMarginResponse(entry model.StopMarginLedger, currents map[uint]model.StopMarginCurrent, currentLedgers map[uint]model.StopMarginLedger) dto.StopMarginLedgerResponse {
	missing := []string{}
	_ = json.Unmarshal([]byte(entry.MissingSourcesJSON), &missing)
	response := dto.StopMarginLedgerResponse{
		ID: entry.ID, MotionProgramID: entry.MotionProgramID,
		ProgramCode: entry.MotionProgram.ProgramCode, ProgramVersion: entry.MotionProgram.Version,
		InputHash: entry.InputHash, InputSnapshot: json.RawMessage(entry.InputSnapshotJSON),
		MaxSpeedMMS: entry.MaxSpeedMMS, MinSeparationMM: entry.MinSeparationMM,
		ResponseTimeMS: entry.ResponseTimeMS, DecelerationMMS2: entry.DecelerationMMS2,
		StoppingDistanceMM: entry.StoppingDistanceMM, MarginMM: entry.MarginMM,
		MissingSources: missing, LedgerStatus: entry.LedgerStatus, Conclusion: entry.Conclusion,
		SettledBy: entry.SettledBy, SettledAt: entry.SettledAt,
	}
	if current, ok := currents[entry.MotionProgramID]; ok {
		response.IsCurrentEffective = current.LedgerID == entry.ID
		if pointed, found := currentLedgers[current.LedgerID]; found {
			response.CurrentEffective = &dto.StopMarginCurrentView{LedgerID: pointed.ID, Conclusion: pointed.Conclusion, SettledAt: pointed.SettledAt}
		}
	}
	return response
}

// programMaxSpeed 从程序版本快照的轨迹中提取最高速度 (mm/s)。
func programMaxSpeed(program model.MotionProgram) (float64, error) {
	var trajectory []dto.TrajectoryPoint
	if err := json.Unmarshal([]byte(program.TrajectoryJSON), &trajectory); err != nil {
		return 0, fmt.Errorf("decode trajectory: %w", err)
	}
	if err := geometry.ValidateTrajectory(trajectory); err != nil {
		return 0, err
	}
	maxSpeed := 0.0
	for _, point := range trajectory {
		if point.SpeedMMS > maxSpeed {
			maxSpeed = point.SpeedMMS
		}
	}
	return maxSpeed, nil
}

func buildStopMarginSnapshot(program model.MotionProgram, maxSpeed float64, request dto.SettleStopMarginRequest) (stopMarginSnapshot, error) {
	responseSources, err := canonicalStopMarginSources(request.ResponseTimeSources)
	if err != nil {
		return stopMarginSnapshot{}, err
	}
	decelerationSources, err := canonicalStopMarginSources(request.DecelerationSources)
	if err != nil {
		return stopMarginSnapshot{}, err
	}
	return stopMarginSnapshot{
		MotionProgramID: program.ID, ProgramCode: program.ProgramCode, ProgramVersion: program.Version,
		SourceChecksum: program.SourceChecksum, MaxSpeedMMS: maxSpeed, MinSeparationMM: request.MinSeparationMM,
		ResponseTimeSources: responseSources, DecelerationSources: decelerationSources,
	}, nil
}

// canonicalStopMarginSources 规范化来源列表：名称去空白、非法值归为缺失、-0 归一化并按
// (名称, 值) 排序，保证同一输入无论来源顺序如何都生成相同的快照与哈希。
func canonicalStopMarginSources(inputs []dto.StopMarginSourceInput) ([]stopMarginSnapshotSource, error) {
	sources := make([]stopMarginSnapshotSource, 0, len(inputs))
	for _, input := range inputs {
		name := strings.TrimSpace(input.Source)
		if name == "" {
			return nil, errors.New("source name must not be blank")
		}
		var value *float64
		if input.Value != nil && stopMarginFinite(*input.Value) {
			normalized := *input.Value
			if normalized == 0 {
				normalized = 0
			}
			value = &normalized
		}
		sources = append(sources, stopMarginSnapshotSource{Source: name, Value: value})
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].Source != sources[j].Source {
			return sources[i].Source < sources[j].Source
		}
		return stopMarginValueKey(sources[i].Value) < stopMarginValueKey(sources[j].Value)
	})
	return sources, nil
}

func stopMarginValueKey(value *float64) float64 {
	if value == nil {
		return math.Inf(-1)
	}
	return *value
}

// resolveStopMarginSources 解析最保守来源值：响应时间取最大、减速度取最小；
// 任一已声明来源缺失或非法时该参数不可结算，整体标为待复核。
func resolveStopMarginSources(snapshot stopMarginSnapshot) stopMarginResolution {
	responseTime, missingResponse := conservativeResponseTime(snapshot.ResponseTimeSources)
	deceleration, missingDeceleration := conservativeDeceleration(snapshot.DecelerationSources)
	missing := append(missingResponse, missingDeceleration...)
	if missing == nil {
		missing = []string{}
	}
	return stopMarginResolution{responseTimeMS: responseTime, decelerationMMS2: deceleration, missing: missing}
}

func conservativeResponseTime(sources []stopMarginSnapshotSource) (*float64, []string) {
	if len(sources) == 0 {
		return nil, []string{"response_time"}
	}
	missing := make([]string, 0)
	var resolved *float64
	for _, source := range sources {
		value, ok := validStopMarginValue(source.Value, true)
		if !ok {
			missing = append(missing, "response_time:"+source.Source)
			continue
		}
		if resolved == nil || value > *resolved {
			candidate := value
			resolved = &candidate
		}
	}
	if len(missing) > 0 {
		return nil, missing
	}
	return resolved, nil
}

func conservativeDeceleration(sources []stopMarginSnapshotSource) (*float64, []string) {
	if len(sources) == 0 {
		return nil, []string{"deceleration"}
	}
	missing := make([]string, 0)
	var resolved *float64
	for _, source := range sources {
		value, ok := validStopMarginValue(source.Value, false)
		if !ok {
			missing = append(missing, "deceleration:"+source.Source)
			continue
		}
		if resolved == nil || value < *resolved {
			candidate := value
			resolved = &candidate
		}
	}
	if len(missing) > 0 {
		return nil, missing
	}
	return resolved, nil
}

func validStopMarginValue(value *float64, allowZero bool) (float64, bool) {
	if value == nil {
		return 0, false
	}
	if allowZero {
		return *value, *value >= 0
	}
	return *value, *value > 0
}

func stopMarginInputHash(snapshotJSON []byte) string {
	sum := sha256.Sum256(snapshotJSON)
	return hex.EncodeToString(sum[:])
}

func stopMarginFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func mustMarshalJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("[]")
	}
	return encoded
}
