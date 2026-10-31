package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func registerGroupPromptRoutes(admin *gin.RouterGroup, h *handler.Handlers, settings *service.SettingService) {
	groups := admin.Group("/groups")
	groups.Use(h.Admin.Group.ScopedAdminKeyGroupModels)
	groups.GET("/:id/prepend-prompt", h.Admin.Group.GroupPrompt(settings))
	groups.PUT("/:id/prepend-prompt", h.Admin.Group.GroupPrompt(settings))
}
