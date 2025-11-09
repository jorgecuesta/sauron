package status

import (
	"context"
	"net/http"
	"strings"

	"sauron/metrics"

	"go.uber.org/zap"
)

// Context key types to avoid collisions
type contextKey string

const (
	contextKeyUser         contextKey = "user"
	contextKeyEnabledTypes contextKey = "enabled_types"
	contextKeyRequestID    contextKey = "request_id"
)

// authMiddleware checks Bearer token authentication
// The key to the Palantír
func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract Bearer token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			h.logger.Warn("Missing Authorization header",
				zap.String("remote_addr", r.RemoteAddr),
			)
			metrics.AuthFailures.WithLabelValues("missing_token").Inc()
			http.Error(w, "Authorization required", http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			h.logger.Warn("Invalid Authorization header format",
				zap.String("remote_addr", r.RemoteAddr),
			)
			metrics.AuthFailures.WithLabelValues("invalid_format").Inc()
			http.Error(w, "Invalid Authorization format. Expected: Bearer <token>", http.StatusUnauthorized)
			return
		}

		token := parts[1]

		// Find user by token
		cfg := h.configLoader.Get()
		user := cfg.FindUser(token)
		if user == nil {
			h.logger.Warn("Invalid token",
				zap.String("remote_addr", r.RemoteAddr),
			)
			metrics.AuthFailures.WithLabelValues("invalid_token").Inc()
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		// Get user's enabled types
		enabledTypes := cfg.GetUserPermissions(token)

		// Add user info to context
		ctx := r.Context()
		ctx = context.WithValue(ctx, contextKeyUser, user.Name)
		ctx = context.WithValue(ctx, contextKeyEnabledTypes, enabledTypes)
		r = r.WithContext(ctx)

		h.logger.Debug("User authenticated",
			zap.String("user", user.Name),
			zap.Strings("enabled_types", enabledTypes),
		)

		// Continue to next handler
		next.ServeHTTP(w, r)
	})
}
