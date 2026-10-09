package middleware

import (
	"context"
	"net/http"
	"strings"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router/catalog"

	"github.com/gin-gonic/gin"
)

// ModelClassGate names the models a strict class order confines a class to;
// *proxy.Service implements it.
type ModelClassGate interface {
	ModelClassMembers(class catalog.Tier) []string
}

// WithModelClass restricts the request's automatic model selection to the
// x-weave-model-class tier, and to the class's strict order when it has one.
// An unknown class is a 400, and so is a class sent with x-weave-force-model:
// the caller asked for both an automatic pick and a fixed model.
func WithModelClass(gate ModelClassGate) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(proxy.ModelClassHeader))
		if raw == "" {
			c.Next()
			return
		}
		class, err := proxy.ParseModelClass(raw)
		if err != nil {
			observability.FromGin(c).Warn("Model class header rejected", "model_class", raw)
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "model_class_header_invalid", "message": err.Error()})
			return
		}
		if strings.TrimSpace(c.GetHeader(proxy.ForceModelHeader)) != "" {
			observability.FromGin(c).Warn("Model class header rejected: request also forces a model", "model_class", class.String())
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error":   "model_class_conflicts_with_force_model",
				"message": proxy.ModelClassHeader + " cannot be combined with " + proxy.ForceModelHeader,
			})
			return
		}
		ctx := context.WithValue(c.Request.Context(), proxy.ModelClassContextKey{}, class)
		if members := gate.ModelClassMembers(class); len(members) > 0 {
			set := make(map[string]struct{}, len(members))
			for _, m := range members {
				set[m] = struct{}{}
			}
			ctx = context.WithValue(ctx, proxy.ModelClassMembersContextKey{}, set)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
