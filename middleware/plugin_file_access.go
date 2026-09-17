package middleware

import (
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// PluginFileAccess is a reusable, expiring read capability. It is deliberately
// not a session or API token: providers may retry and make HEAD/Range requests.
func PluginFileAccess() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "private, no-store")
		access := c.GetString(taskArtifactAccessRawContextKey)
		invalid := c.GetBool(taskArtifactAccessInvalidContextKey)
		if queryAccess, present, malformed := popTaskArtifactAccessQuery(c.Request); present {
			if access == "" {
				access = queryAccess
			}
			invalid = invalid || malformed
		}
		object := c.Param("object")
		if invalid || !service.VerifyPluginFileAccess(object, access, time.Now()) {
			status := http.StatusNotFound
			if !taskArtifactAnonymousLimiter.invalidAttempt(time.Now(), c.ClientIP()) {
				status = http.StatusTooManyRequests
			}
			c.AbortWithStatus(status)
			return
		}
		release, ok := taskArtifactAnonymousLimiter.acquire(c.ClientIP(), "plugin-input", object)
		if !ok {
			c.AbortWithStatus(http.StatusTooManyRequests)
			return
		}
		defer release()
		c.Next()
	}
}
