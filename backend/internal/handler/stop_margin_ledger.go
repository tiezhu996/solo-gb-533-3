package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"robot-cell-safety-envelope-validator/backend/internal/dto"
	"robot-cell-safety-envelope-validator/backend/internal/service"
)

type StopMarginLedgerHandler struct {
	service *service.StopMarginLedgerService
}

func NewStopMarginLedgerHandler(service *service.StopMarginLedgerService) *StopMarginLedgerHandler {
	return &StopMarginLedgerHandler{service: service}
}

// Settle 是安全停机余量台账唯一的执行入口。
func (handler *StopMarginLedgerHandler) Settle(context *gin.Context) {
	var request dto.SettleStopMarginRequest
	if err := BindAndValidate(context, &request); err != nil {
		WriteError(context, err)
		return
	}
	item, reused, err := handler.service.Settle(request, Actor(context), RequestID(context))
	if err != nil {
		WriteError(context, err)
		return
	}
	status := http.StatusCreated
	if reused {
		status = http.StatusOK
	}
	WriteData(context, status, item)
}

func (handler *StopMarginLedgerHandler) List(context *gin.Context) {
	page, pageSize := Pagination(context)
	items, meta, err := handler.service.List(page, pageSize, QueryUint(context, "motion_program_id"), context.Query("status"))
	if err != nil {
		WriteError(context, err)
		return
	}
	WritePage(context, items, meta)
}

func (handler *StopMarginLedgerHandler) Get(context *gin.Context) {
	id, err := PathID(context)
	if err != nil {
		WriteError(context, err)
		return
	}
	item, err := handler.service.Get(id)
	if err != nil {
		WriteError(context, err)
		return
	}
	WriteData(context, http.StatusOK, item)
}
