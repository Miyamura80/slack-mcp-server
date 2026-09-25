package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/korotovsky/slack-mcp-server/pkg/limiter"
	"github.com/rusq/slackdump/v3/auth"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// IsBotToken reports whether token is a Slack bot token (xoxb, or its
// token-rotation variant xoxe.xoxb).
func IsBotToken(token string) bool {
	return strings.HasPrefix(token, "xoxb-") || strings.HasPrefix(token, "xoxe.xoxb-")
}

// TokenKey returns a stable, non-reversible identifier for a token, used to
// key per-tenant state and cache files without ever writing the token itself.
func TokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:32]
}

// NewForBotToken builds a provider for one bot token supplied at request time
// (multi-tenant HTTP mode). Unlike New, it never reads token env vars and
// returns errors instead of exiting, since a bad token from one caller must
// not take the server down for everyone else.
//
// Cache files are keyed by the token rather than the team: two bots in the
// same workspace can see different channels, so they must not share a cache.
func NewForBotToken(transport, token string, logger *zap.Logger) (*ApiProvider, error) {
	if !IsBotToken(token) {
		return nil, fmt.Errorf("not a bot token (expected xoxb-...)")
	}

	authProvider, err := auth.NewValueAuth(token, "")
	if err != nil {
		return nil, fmt.Errorf("invalid bot token: %w", err)
	}

	key := TokenKey(token)
	logger = logger.With(zap.String("tenant", key[:12]))

	if _, err := validateAuthAndGetTeamID(authProvider, logger); err != nil {
		return nil, fmt.Errorf("slack auth.test failed: %w", err)
	}

	client, err := NewMCPSlackClient(authProvider, logger)
	if err != nil {
		return nil, fmt.Errorf("create slack client: %w", err)
	}

	ap := &ApiProvider{
		transport: transport,
		client:    client,
		logger:    logger,

		rateLimiter:        limiter.Tier2.Limiter(),
		cacheTTL:           getCacheTTL(),
		minRefreshInterval: getMinRefreshInterval(),

		usersCachePath:    getCachePathWithTeamID("bot-"+key, "users_cache.json"),
		channelsCachePath: getCachePathWithTeamID("bot-"+key, "channels_cache_v2.json"),
	}
	ap.usersSnapshot.Store(&UsersCache{
		Users:    make(map[string]slack.User),
		UsersInv: make(map[string]string),
	})
	ap.channelsSnapshot.Store(&ChannelsCache{
		Channels:    make(map[string]Channel),
		ChannelsInv: make(map[string]string),
	})
	return ap, nil
}
