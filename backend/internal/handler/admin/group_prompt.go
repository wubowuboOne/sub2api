package admin

import (
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// GroupPrompt keeps this optional configuration separate from the group DTO.
func (h *GroupHandler) GroupPrompt(settings *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.rejectUnsupportedSimpleModeOperation(c, "advanced") {
			return
		}
		if adminaccess.FromContext(c.Request.Context()) != nil {
			response.Forbidden(c, "Group prompt settings require an administrator session")
			return
		}
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid group ID")
			return
		}
		if _, err := h.adminService.GetGroup(c.Request.Context(), id); err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if settings == nil {
			response.Error(c, http.StatusServiceUnavailable, "Group prompt storage is unavailable")
			return
		}
		var config groupprompt.Config
		if c.Request.Method == http.MethodPut {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<20)
			if err := c.ShouldBindJSON(&config); err != nil {
				response.BadRequest(c, "Invalid group prompt configuration")
				return
			}
			if err := config.Validate(); err != nil {
				response.BadRequest(c, err.Error())
				return
			}
			if err := settings.SetGroupPrompt(c.Request.Context(), id, config); err != nil {
				response.ErrorFrom(c, err)
				return
			}
		} else {
			config, err = settings.GetGroupPrompt(c.Request.Context(), id)
			if err != nil {
				response.ErrorFrom(c, err)
				return
			}
		}
		response.Success(c, gin.H{"enabled": config.Enabled, "content": config.Content, "model_prompts": config.ModelPrompts, "default_content": groupprompt.DefaultPrompt})
	}
}
