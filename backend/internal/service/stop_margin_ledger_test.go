package service

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"robot-cell-safety-envelope-validator/backend/internal/constants"
	"robot-cell-safety-envelope-validator/backend/internal/dto"
	"robot-cell-safety-envelope-validator/backend/internal/model"
	"robot-cell-safety-envelope-validator/backend/internal/repository"
)

type stopMarginFixture struct {
	service *StopMarginLedgerService
	db      *gorm.DB
	actor   dto.Actor
	program model.MotionProgram
}

func newStopMarginFixture(t *testing.T) stopMarginFixture {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL", filepath.Join(t.TempDir(), "stop-margin-test.db"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.RobotCell{}, &model.MotionProgram{}, &model.StopMarginLedger{}, &model.StopMarginCurrent{}, &model.AuditEvent{}); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	user := model.User{Username: "engineer", PasswordHash: "hash", Role: constants.RoleSafetyEngineer, Active: true}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	cell := model.RobotCell{
		CellCode: "CELL-T1", Name: "ledger test cell", LayoutGeoJSON: "{}", RobotModel: "R1", ControllerModel: "C1",
		MaxReachMM: 1000, OwnerTeam: "safety", CellState: constants.CellStateFrozen, LayoutVersion: 1, CreatedBy: user.ID,
	}
	if err := db.Create(&cell).Error; err != nil {
		t.Fatalf("seed cell: %v", err)
	}
	program := seedStopMarginProgram(t, db, cell.ID, user.ID, "PROG-1", 1, constants.ProgramStateReady, 250, 500, 400)
	systemService := NewSystemService(repository.NewSystemRepository(db), "test-secret-with-24-plus-bytes", time.Hour)
	ledgerService := NewStopMarginLedgerService(db, repository.NewStopMarginLedgerRepository(db), repository.NewMotionProgramRepository(db), systemService)
	actor := dto.Actor{ID: user.ID, Username: user.Username, Role: user.Role}
	return stopMarginFixture{service: ledgerService, db: db, actor: actor, program: program}
}

func seedStopMarginProgram(t *testing.T, db *gorm.DB, cellID, uploaderID uint, code string, version int, state string, speeds ...float64) model.MotionProgram {
	t.Helper()
	trajectory := make([]dto.TrajectoryPoint, 0, len(speeds))
	for index, speed := range speeds {
		trajectory = append(trajectory, dto.TrajectoryPoint{XMM: float64(index * 100), YMM: 0, ZMM: 100, TimeMS: float64(index * 1000), SpeedMMS: speed})
	}
	trajectoryJSON, _ := json.Marshal(trajectory)
	program := model.MotionProgram{
		RobotCellID: cellID, ProgramCode: code, Version: version, TrajectoryJSON: string(trajectoryJSON),
		ToolRadiusMM: 100, PayloadRadiusMM: 50, InterlockSequenceJSON: "[]",
		SourceChecksum: fmt.Sprintf("checksum-%s-v%d", code, version), ProgramState: state,
		UploadedBy: uploaderID, UploadedAt: time.Now().UTC(),
	}
	if err := db.Create(&program).Error; err != nil {
		t.Fatalf("seed program: %v", err)
	}
	return program
}

func floatPtr(value float64) *float64 { return &value }

// 基准请求：响应时间来源 30/45ms，减速度来源 1200/900 mm/s²，程序最高速度 500 mm/s。
func stopMarginRequest(programID uint, minSeparation float64) dto.SettleStopMarginRequest {
	return dto.SettleStopMarginRequest{
		MotionProgramID: programID,
		MinSeparationMM: minSeparation,
		ResponseTimeSources: []dto.StopMarginSourceInput{
			{Source: "controller_spec", Value: floatPtr(30)},
			{Source: "measured", Value: floatPtr(45)},
		},
		DecelerationSources: []dto.StopMarginSourceInput{
			{Source: "brake_manual", Value: floatPtr(1200)},
			{Source: "measured", Value: floatPtr(900)},
		},
	}
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func ledgerCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	if err := db.Model(&model.StopMarginLedger{}).Count(&count).Error; err != nil {
		t.Fatalf("count ledgers: %v", err)
	}
	return count
}

func auditCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	if err := db.Model(&model.AuditEvent{}).Where("action = ?", "stop_margin_ledger.settled").Count(&count).Error; err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	return count
}

func TestSettleComputesConservativeMargin(t *testing.T) {
	fixture := newStopMarginFixture(t)
	response, reused, err := fixture.service.Settle(stopMarginRequest(fixture.program.ID, 500), fixture.actor, "req-valid")
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if reused {
		t.Fatal("first settlement must not be reused")
	}
	if response.LedgerStatus != constants.LedgerStatusValid {
		t.Fatalf("ledger status = %s, want valid", response.LedgerStatus)
	}
	// 最保守取值：响应时间取最大 45ms，减速度取最小 900 mm/s²。
	if response.ResponseTimeMS == nil || !almostEqual(*response.ResponseTimeMS, 45) {
		t.Fatalf("response time = %v, want 45", response.ResponseTimeMS)
	}
	if response.DecelerationMMS2 == nil || !almostEqual(*response.DecelerationMMS2, 900) {
		t.Fatalf("deceleration = %v, want 900", response.DecelerationMMS2)
	}
	// 停机距离 S = v·t + v²/(2a) = 500·0.045 + 500²/1800。
	wantStopping := 500*0.045 + 500*500/1800.0
	if response.StoppingDistanceMM == nil || !almostEqual(*response.StoppingDistanceMM, wantStopping) {
		t.Fatalf("stopping distance = %v, want %v", response.StoppingDistanceMM, wantStopping)
	}
	if response.MarginMM == nil || !almostEqual(*response.MarginMM, 500-wantStopping) {
		t.Fatalf("margin = %v, want %v", response.MarginMM, 500-wantStopping)
	}
	if response.Conclusion != constants.LedgerConclusionSufficient {
		t.Fatalf("conclusion = %s, want sufficient", response.Conclusion)
	}
	if response.MaxSpeedMMS != 500 {
		t.Fatalf("max speed = %v, want 500 (trajectory maximum)", response.MaxSpeedMMS)
	}
	if !response.IsCurrentEffective || response.CurrentEffective == nil || response.CurrentEffective.LedgerID != response.ID {
		t.Fatal("new valid ledger must become the current effective conclusion")
	}
	if len(response.MissingSources) != 0 {
		t.Fatalf("missing sources = %v, want empty", response.MissingSources)
	}
	if auditCount(t, fixture.db) != 1 {
		t.Fatal("settlement must record exactly one audit event")
	}
}

func TestSettleInsufficientMargin(t *testing.T) {
	fixture := newStopMarginFixture(t)
	response, _, err := fixture.service.Settle(stopMarginRequest(fixture.program.ID, 100), fixture.actor, "req-insufficient")
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if response.Conclusion != constants.LedgerConclusionInsufficient {
		t.Fatalf("conclusion = %s, want insufficient", response.Conclusion)
	}
	if response.MarginMM == nil || *response.MarginMM >= 0 {
		t.Fatalf("margin = %v, want negative", response.MarginMM)
	}
}

func TestSettlePendingReviewOnMissingSource(t *testing.T) {
	fixture := newStopMarginFixture(t)
	request := stopMarginRequest(fixture.program.ID, 500)
	request.ResponseTimeSources[1].Value = nil
	response, _, err := fixture.service.Settle(request, fixture.actor, "req-pending")
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if response.LedgerStatus != constants.LedgerStatusPendingReview {
		t.Fatalf("ledger status = %s, want pending_review", response.LedgerStatus)
	}
	if response.Conclusion != "" || response.ResponseTimeMS != nil || response.DecelerationMMS2 != nil || response.StoppingDistanceMM != nil || response.MarginMM != nil {
		t.Fatal("pending ledger must not settle distance, margin, or conclusion")
	}
	if len(response.MissingSources) != 1 || response.MissingSources[0] != "response_time:measured" {
		t.Fatalf("missing sources = %v, want [response_time:measured]", response.MissingSources)
	}
	if response.IsCurrentEffective || response.CurrentEffective != nil {
		t.Fatal("pending ledger must not become the current effective conclusion")
	}
}

func TestSettlePendingReviewOnEmptyAndInvalidSources(t *testing.T) {
	fixture := newStopMarginFixture(t)
	emptyDecel := stopMarginRequest(fixture.program.ID, 500)
	emptyDecel.DecelerationSources = nil
	response, _, err := fixture.service.Settle(emptyDecel, fixture.actor, "req-empty")
	if err != nil {
		t.Fatalf("settle empty sources: %v", err)
	}
	if response.LedgerStatus != constants.LedgerStatusPendingReview || len(response.MissingSources) != 1 || response.MissingSources[0] != "deceleration" {
		t.Fatalf("empty deceleration sources: status=%s missing=%v", response.LedgerStatus, response.MissingSources)
	}
	invalidValue := stopMarginRequest(fixture.program.ID, 500)
	invalidValue.DecelerationSources[0].Value = floatPtr(0)
	invalidValue.DecelerationSources[1].Value = floatPtr(-50)
	response, _, err = fixture.service.Settle(invalidValue, fixture.actor, "req-invalid")
	if err != nil {
		t.Fatalf("settle invalid values: %v", err)
	}
	if response.LedgerStatus != constants.LedgerStatusPendingReview || len(response.MissingSources) != 2 {
		t.Fatalf("non-positive deceleration must be treated as missing: status=%s missing=%v", response.LedgerStatus, response.MissingSources)
	}
}

func TestPendingDoesNotOverwriteValidConclusion(t *testing.T) {
	fixture := newStopMarginFixture(t)
	valid, _, err := fixture.service.Settle(stopMarginRequest(fixture.program.ID, 500), fixture.actor, "req-valid-first")
	if err != nil {
		t.Fatalf("settle valid: %v", err)
	}
	pendingRequest := stopMarginRequest(fixture.program.ID, 500)
	pendingRequest.ResponseTimeSources[1].Value = nil
	pending, _, err := fixture.service.Settle(pendingRequest, fixture.actor, "req-pending-second")
	if err != nil {
		t.Fatalf("settle pending: %v", err)
	}
	if pending.LedgerStatus != constants.LedgerStatusPendingReview {
		t.Fatalf("second ledger status = %s, want pending_review", pending.LedgerStatus)
	}
	if pending.IsCurrentEffective {
		t.Fatal("pending ledger must not become the current effective conclusion")
	}
	if pending.CurrentEffective == nil || pending.CurrentEffective.LedgerID != valid.ID {
		t.Fatal("existing valid conclusion must remain the current effective conclusion")
	}
	reloaded, err := fixture.service.Get(valid.ID)
	if err != nil {
		t.Fatalf("get valid ledger: %v", err)
	}
	if !reloaded.IsCurrentEffective {
		t.Fatal("valid ledger must still be the current effective conclusion after a pending settlement")
	}
	if ledgerCount(t, fixture.db) != 2 {
		t.Fatal("both settlements must be recorded as ledger entries")
	}
}

func TestValidAdvancesCurrentAndStaleReplayDoesNotRollBack(t *testing.T) {
	fixture := newStopMarginFixture(t)
	first, _, err := fixture.service.Settle(stopMarginRequest(fixture.program.ID, 500), fixture.actor, "req-first")
	if err != nil {
		t.Fatalf("settle first: %v", err)
	}
	second, _, err := fixture.service.Settle(stopMarginRequest(fixture.program.ID, 100), fixture.actor, "req-second")
	if err != nil {
		t.Fatalf("settle second: %v", err)
	}
	if second.Conclusion != constants.LedgerConclusionInsufficient || !second.IsCurrentEffective {
		t.Fatal("new valid ledger must advance the current effective conclusion")
	}
	reloadedFirst, err := fixture.service.Get(first.ID)
	if err != nil {
		t.Fatalf("get first ledger: %v", err)
	}
	if reloadedFirst.IsCurrentEffective {
		t.Fatal("superseded valid ledger must no longer be current, but stays readable")
	}
	replay, reused, err := fixture.service.Settle(stopMarginRequest(fixture.program.ID, 500), fixture.actor, "req-replay")
	if err != nil {
		t.Fatalf("replay first input: %v", err)
	}
	if !reused || replay.ID != first.ID {
		t.Fatal("replaying a historical input must reuse the original ledger entry")
	}
	reloadedSecond, err := fixture.service.Get(second.ID)
	if err != nil {
		t.Fatalf("get second ledger: %v", err)
	}
	if !reloadedSecond.IsCurrentEffective {
		t.Fatal("replaying a stale input must not roll back the current effective conclusion")
	}
}

func TestSettleIdempotentReplay(t *testing.T) {
	fixture := newStopMarginFixture(t)
	request := stopMarginRequest(fixture.program.ID, 500)
	first, reused, err := fixture.service.Settle(request, fixture.actor, "req-a")
	if err != nil || reused {
		t.Fatalf("first settle: reused=%v err=%v", reused, err)
	}
	second, reused, err := fixture.service.Settle(request, fixture.actor, "req-b")
	if err != nil {
		t.Fatalf("replay settle: %v", err)
	}
	if !reused || second.ID != first.ID {
		t.Fatalf("repeated submission must reuse the same ledger entry, got id=%d reused=%v", second.ID, reused)
	}
	if ledgerCount(t, fixture.db) != 1 {
		t.Fatal("repeated submission must not create additional ledger entries")
	}
	if auditCount(t, fixture.db) != 1 {
		t.Fatal("reused submission must not record another audit event")
	}
}

func TestSettleIdempotentAcrossSourceOrder(t *testing.T) {
	fixture := newStopMarginFixture(t)
	first, _, err := fixture.service.Settle(stopMarginRequest(fixture.program.ID, 500), fixture.actor, "req-order-a")
	if err != nil {
		t.Fatalf("first settle: %v", err)
	}
	reordered := stopMarginRequest(fixture.program.ID, 500)
	reordered.ResponseTimeSources[0], reordered.ResponseTimeSources[1] = reordered.ResponseTimeSources[1], reordered.ResponseTimeSources[0]
	reordered.DecelerationSources[0], reordered.DecelerationSources[1] = reordered.DecelerationSources[1], reordered.DecelerationSources[0]
	second, reused, err := fixture.service.Settle(reordered, fixture.actor, "req-order-b")
	if err != nil {
		t.Fatalf("reordered settle: %v", err)
	}
	if !reused || second.ID != first.ID || second.InputHash != first.InputHash {
		t.Fatal("source order must not change the input hash or create a new ledger entry")
	}
	if ledgerCount(t, fixture.db) != 1 {
		t.Fatal("reordered identical input must not create additional ledger entries")
	}
}

func TestSettleConcurrentSameInput(t *testing.T) {
	fixture := newStopMarginFixture(t)
	request := stopMarginRequest(fixture.program.ID, 500)
	const workers = 16
	type outcome struct {
		response dto.StopMarginLedgerResponse
		reused   bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan outcome, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			response, reused, err := fixture.service.Settle(request, fixture.actor, fmt.Sprintf("req-concurrent-%d", i))
			results <- outcome{response, reused, err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	created, ledgerID := 0, uint(0)
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent settle: %v", result.err)
		}
		if !result.reused {
			created++
		}
		if ledgerID == 0 {
			ledgerID = result.response.ID
		} else if result.response.ID != ledgerID {
			t.Fatalf("concurrent submissions produced different ledger entries: %d and %d", ledgerID, result.response.ID)
		}
	}
	if created != 1 {
		t.Fatalf("exactly one concurrent submission may create the ledger, got %d", created)
	}
	if ledgerCount(t, fixture.db) != 1 {
		t.Fatal("concurrent identical submissions must produce exactly one ledger entry")
	}
}

func TestProgramUpdateKeepsHistoryReadOnly(t *testing.T) {
	fixture := newStopMarginFixture(t)
	original, _, err := fixture.service.Settle(stopMarginRequest(fixture.program.ID, 500), fixture.actor, "req-v1")
	if err != nil {
		t.Fatalf("settle v1: %v", err)
	}
	// 程序更新：同一代码的新版本是新的程序记录，结算产生新的台账，旧台账保持只读。
	programV2 := seedStopMarginProgram(t, fixture.db, fixture.program.RobotCellID, fixture.actor.ID, "PROG-1", 2, constants.ProgramStateReady, 300, 300)
	updated, _, err := fixture.service.Settle(stopMarginRequest(programV2.ID, 500), fixture.actor, "req-v2")
	if err != nil {
		t.Fatalf("settle v2: %v", err)
	}
	if updated.ID == original.ID || updated.InputHash == original.InputHash {
		t.Fatal("updated program parameters must produce a new ledger entry")
	}
	if updated.MaxSpeedMMS != 300 {
		t.Fatalf("v2 max speed = %v, want 300", updated.MaxSpeedMMS)
	}
	reloaded, err := fixture.service.Get(original.ID)
	if err != nil {
		t.Fatalf("get original ledger: %v", err)
	}
	if reloaded.MaxSpeedMMS != 500 || reloaded.LedgerStatus != constants.LedgerStatusValid || !reloaded.IsCurrentEffective {
		t.Fatal("historical ledger of the previous program version must remain intact and effective")
	}
	if reloaded.CurrentEffective == nil || reloaded.CurrentEffective.LedgerID != original.ID {
		t.Fatal("current conclusion of the old program version must not be touched by the new version")
	}
	entries, meta, err := fixture.service.List(1, 20, fixture.program.ID, "")
	if err != nil {
		t.Fatalf("list ledgers: %v", err)
	}
	if meta.Total != 1 || len(entries) != 1 || entries[0].ID != original.ID {
		t.Fatal("old program version must keep exactly its own ledger history")
	}
}

func TestSettleRejectsNonReadyProgram(t *testing.T) {
	fixture := newStopMarginFixture(t)
	uploaded := seedStopMarginProgram(t, fixture.db, fixture.program.RobotCellID, fixture.actor.ID, "PROG-2", 1, constants.ProgramStateUploaded, 100, 100)
	_, _, err := fixture.service.Settle(stopMarginRequest(uploaded.ID, 500), fixture.actor, "req-state")
	appError, ok := err.(*AppError)
	if !ok || appError.Status != 409 {
		t.Fatalf("settle on uploaded program: err=%v, want 409 conflict", err)
	}
}

func TestSettleUnknownProgram(t *testing.T) {
	fixture := newStopMarginFixture(t)
	_, _, err := fixture.service.Settle(stopMarginRequest(99999, 500), fixture.actor, "req-unknown")
	appError, ok := err.(*AppError)
	if !ok || appError.Status != 404 {
		t.Fatalf("settle on unknown program: err=%v, want 404 not found", err)
	}
}

func TestSettleNonFiniteResultPending(t *testing.T) {
	fixture := newStopMarginFixture(t)
	request := stopMarginRequest(fixture.program.ID, 500)
	request.DecelerationSources[1].Value = floatPtr(1e-320)
	response, _, err := fixture.service.Settle(request, fixture.actor, "req-nonfinite")
	if err != nil {
		t.Fatalf("settle non-finite: %v", err)
	}
	if response.LedgerStatus != constants.LedgerStatusPendingReview {
		t.Fatalf("non-finite stopping distance must be marked pending_review, got %s", response.LedgerStatus)
	}
	if len(response.MissingSources) != 1 || response.MissingSources[0] != "settlement:non_finite_result" {
		t.Fatalf("missing sources = %v, want [settlement:non_finite_result]", response.MissingSources)
	}
}
