package model

import "time"

// StopMarginLedger 安全停机余量台账。台账仅追加、不提供任何更新或删除入口，
// 程序或设备参数更新只会产生新的台账记录，历史记录保持只读。
type StopMarginLedger struct {
	ID                 uint    `gorm:"primaryKey"`
	MotionProgramID    uint    `gorm:"index;not null"`
	InputHash          string  `gorm:"size:64;uniqueIndex;not null"`
	InputSnapshotJSON  string  `gorm:"type:text;not null"`
	MaxSpeedMMS        float64 `gorm:"not null"`
	MinSeparationMM    float64 `gorm:"not null"`
	ResponseTimeMS     *float64
	DecelerationMMS2   *float64
	StoppingDistanceMM *float64
	MarginMM           *float64
	MissingSourcesJSON string        `gorm:"type:text;not null"`
	LedgerStatus       string        `gorm:"size:24;index;not null"`
	Conclusion         string        `gorm:"size:24;index;not null"`
	SettledBy          uint          `gorm:"index;not null"`
	SettledAt          time.Time     `gorm:"index;not null"`
	MotionProgram      MotionProgram `gorm:"foreignKey:MotionProgramID"`
}

// StopMarginCurrent 记录每个运动程序当前生效的有效结论指针。
// 仅当新台账为 valid 时在结算事务内推进；pending_review 台账不得覆盖已有有效结论。
type StopMarginCurrent struct {
	MotionProgramID uint      `gorm:"primaryKey"`
	LedgerID        uint      `gorm:"not null"`
	UpdatedAt       time.Time `gorm:"not null"`
}
