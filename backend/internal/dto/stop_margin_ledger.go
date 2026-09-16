package dto

import (
	"encoding/json"
	"time"
)

// StopMarginSourceInput 单一参数来源；value 为空或非法表示该来源缺失。
type StopMarginSourceInput struct {
	Source string   `json:"source" validate:"required,max=80"`
	Value  *float64 `json:"value"`
}

// SettleStopMarginRequest 台账结算请求，是本模块唯一执行入口的输入。
// 响应时间来源单位 ms，制动减速度来源单位 mm/s^2。
type SettleStopMarginRequest struct {
	MotionProgramID     uint                    `json:"motion_program_id" validate:"required"`
	MinSeparationMM     float64                 `json:"min_separation_mm" validate:"required,gt=0"`
	ResponseTimeSources []StopMarginSourceInput `json:"response_time_sources" validate:"max=16"`
	DecelerationSources []StopMarginSourceInput `json:"deceleration_sources" validate:"max=16"`
}

// StopMarginCurrentView 程序当前生效的有效结论摘要。
type StopMarginCurrentView struct {
	LedgerID   uint      `json:"ledger_id"`
	Conclusion string    `json:"conclusion"`
	SettledAt  time.Time `json:"settled_at"`
}

type StopMarginLedgerResponse struct {
	ID                 uint                   `json:"id"`
	MotionProgramID    uint                   `json:"motion_program_id"`
	ProgramCode        string                 `json:"program_code"`
	ProgramVersion     int                    `json:"program_version"`
	InputHash          string                 `json:"input_hash"`
	InputSnapshot      json.RawMessage        `json:"input_snapshot"`
	MaxSpeedMMS        float64                `json:"max_speed_mm_s"`
	MinSeparationMM    float64                `json:"min_separation_mm"`
	ResponseTimeMS     *float64               `json:"response_time_ms"`
	DecelerationMMS2   *float64               `json:"deceleration_mm_s2"`
	StoppingDistanceMM *float64               `json:"stopping_distance_mm"`
	MarginMM           *float64               `json:"margin_mm"`
	MissingSources     []string               `json:"missing_sources"`
	LedgerStatus       string                 `json:"ledger_status"`
	Conclusion         string                 `json:"conclusion"`
	IsCurrentEffective bool                   `json:"is_current_effective"`
	CurrentEffective   *StopMarginCurrentView `json:"current_effective"`
	SettledBy          uint                   `json:"settled_by"`
	SettledAt          time.Time              `json:"settled_at"`
	Reused             bool                   `json:"reused"`
}
