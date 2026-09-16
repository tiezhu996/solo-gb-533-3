package router

import (
	"github.com/gin-gonic/gin"

	"robot-cell-safety-envelope-validator/backend/internal/constants"
	"robot-cell-safety-envelope-validator/backend/internal/handler"
	"robot-cell-safety-envelope-validator/backend/internal/middleware"
)

// registerStopMarginLedgerRoutes 台账只登记一个 POST 执行入口，其余均为只读查询；
// 不提供更新、删除、评审等任何会改写历史的端点。
func registerStopMarginLedgerRoutes(group *gin.RouterGroup, target *handler.StopMarginLedgerHandler) {
	routes := group.Group("/stop-margin-ledgers")
	routes.GET("", target.List)
	routes.GET("/:id", target.Get)
	routes.POST("", middleware.RBAC(constants.RoleSafetyEngineer, constants.RoleAdmin), middleware.RateLimit(30, "stop-margin-settle"), target.Settle)
}
