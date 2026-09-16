package repository

import (
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"robot-cell-safety-envelope-validator/backend/internal/model"
)

// StopMarginLedgerRepository 只提供追加与查询；台账没有更新和删除入口，历史记录保持只读。
type StopMarginLedgerRepository struct{ db *gorm.DB }

func NewStopMarginLedgerRepository(db *gorm.DB) *StopMarginLedgerRepository {
	return &StopMarginLedgerRepository{db: db}
}
func (repository *StopMarginLedgerRepository) WithDB(db *gorm.DB) *StopMarginLedgerRepository {
	return &StopMarginLedgerRepository{db: db}
}

func (repository *StopMarginLedgerRepository) Create(entry *model.StopMarginLedger) error {
	if err := repository.db.Create(entry).Error; err != nil {
		return fmt.Errorf("create stop margin ledger: %w", err)
	}
	return nil
}

func (repository *StopMarginLedgerRepository) Get(id uint) (model.StopMarginLedger, error) {
	var entry model.StopMarginLedger
	if err := repository.db.Preload("MotionProgram").First(&entry, id).Error; err != nil {
		return entry, fmt.Errorf("get stop margin ledger: %w", err)
	}
	return entry, nil
}

func (repository *StopMarginLedgerRepository) LedgersByIDs(ids []uint) (map[uint]model.StopMarginLedger, error) {
	entries := make(map[uint]model.StopMarginLedger, len(ids))
	if len(ids) == 0 {
		return entries, nil
	}
	var rows []model.StopMarginLedger
	if err := repository.db.Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list stop margin ledgers by id: %w", err)
	}
	for _, row := range rows {
		entries[row.ID] = row
	}
	return entries, nil
}

func (repository *StopMarginLedgerRepository) FindByInputHash(inputHash string) (model.StopMarginLedger, error) {
	var entry model.StopMarginLedger
	if err := repository.db.Preload("MotionProgram").Where("input_hash = ?", inputHash).First(&entry).Error; err != nil {
		return entry, fmt.Errorf("find stop margin ledger by input hash: %w", err)
	}
	return entry, nil
}

func (repository *StopMarginLedgerRepository) List(page, pageSize int, programID uint, status string) ([]model.StopMarginLedger, int64, error) {
	query := repository.db.Model(&model.StopMarginLedger{})
	if programID > 0 {
		query = query.Where("motion_program_id = ?", programID)
	}
	if status != "" {
		query = query.Where("ledger_status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count stop margin ledgers: %w", err)
	}
	var entries []model.StopMarginLedger
	if err := query.Preload("MotionProgram").Order("settled_at DESC, id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&entries).Error; err != nil {
		return nil, 0, fmt.Errorf("list stop margin ledgers: %w", err)
	}
	return entries, total, nil
}

// AdvanceCurrent 把程序当前有效结论推进到一条新的有效台账，仅允许在结算事务内调用；
// 待复核台账永远不会调用它，因此不会覆盖已有有效结论。
func (repository *StopMarginLedgerRepository) AdvanceCurrent(current *model.StopMarginCurrent) error {
	if err := repository.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "motion_program_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"ledger_id", "updated_at"}),
	}).Create(current).Error; err != nil {
		return fmt.Errorf("advance current stop margin conclusion: %w", err)
	}
	return nil
}

func (repository *StopMarginLedgerRepository) CurrentForPrograms(programIDs []uint) (map[uint]model.StopMarginCurrent, error) {
	currents := make(map[uint]model.StopMarginCurrent, len(programIDs))
	if len(programIDs) == 0 {
		return currents, nil
	}
	var rows []model.StopMarginCurrent
	if err := repository.db.Where("motion_program_id IN ?", programIDs).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list current stop margin conclusions: %w", err)
	}
	for _, row := range rows {
		currents[row.MotionProgramID] = row
	}
	return currents, nil
}
