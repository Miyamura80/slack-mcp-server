package main

import (
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/korotovsky/slack-mcp-server/pkg/server"
	"go.uber.org/zap"
)

const defaultMaxTenants = 200
const tenantReadyWait = 15 * time.Second

// multiTenantEnabled reports whether SLACK_MCP_MULTI_TENANT is on. In that mode
// the server holds no token of its own: every request brings its bot token in
// the X-Slack-Bot-Token header, and one process serves any number of bots.
func multiTenantEnabled() bool {
	v := os.Getenv("SLACK_MCP_MULTI_TENANT")
	return v == "true" || v == "1"
}

func runMultiTenant(transport string, enabledTools []string, noCache bool, logger *zap.Logger) {
	if transport != "http" {
		logger.Fatal("SLACK_MCP_MULTI_TENANT requires --transport http",
			zap.String("context", "console"),
			zap.String("transport", transport),
		)
	}

	// Callers hand us live Slack tokens, so an unauthenticated endpoint would
	// let anyone who finds the URL borrow the server. Refuse to start without
	// a key rather than fall back to open access as single-tenant mode does.
	apiKey := os.Getenv("SLACK_MCP_API_KEY")
	if apiKey == "" {
		logger.Fatal("SLACK_MCP_MULTI_TENANT requires SLACK_MCP_API_KEY",
			zap.String("context", "console"),
		)
	}

	maxTenants := defaultMaxTenants
	if v := os.Getenv("SLACK_MCP_MAX_TENANTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			logger.Fatal("SLACK_MCP_MAX_TENANTS must be a positive integer",
				zap.String("context", "console"),
				zap.String("value", v),
			)
		}
		maxTenants = n
	}

	host := os.Getenv("SLACK_MCP_HOST")
	if host == "" {
		host = defaultSseHost
	}
	port := os.Getenv("SLACK_MCP_PORT")
	if port == "" {
		port = strconv.Itoa(defaultSsePort)
	}

	router := server.NewTenantRouter(
		apiKey,
		maxTenants,
		server.NewBotTenantFactory(enabledTools, noCache, tenantReadyWait, logger),
		logger,
	)

	logger.Info("Multi-tenant HTTP server listening",
		zap.String("context", "console"),
		zap.String("address", host+":"+port+"/mcp"),
		zap.String("token_header", server.TenantTokenHeader),
		zap.Int("max_tenants", maxTenants),
	)

	srv := &http.Server{
		Addr:              host + ":" + port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		logger.Fatal("Server error",
			zap.String("context", "console"),
			zap.Error(err),
		)
	}
}
