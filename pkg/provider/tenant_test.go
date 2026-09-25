package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestUnitIsBotToken(t *testing.T) {
	assert.True(t, IsBotToken("xoxb-123"))
	assert.True(t, IsBotToken("xoxe.xoxb-123"))
	assert.False(t, IsBotToken("xoxp-123"))
	assert.False(t, IsBotToken("xoxc-123"))
	assert.False(t, IsBotToken(""))
}

func TestUnitTokenKey_StableAndDoesNotLeakToken(t *testing.T) {
	k := TokenKey("xoxb-secret")
	assert.Equal(t, k, TokenKey("xoxb-secret"))
	assert.NotEqual(t, k, TokenKey("xoxb-other"))
	assert.Len(t, k, 32)
	assert.NotContains(t, k, "secret")
}

func TestUnitNewForBotToken_RejectsNonBotTokenWithoutCallingSlack(t *testing.T) {
	_, err := NewForBotToken("http", "xoxp-user-token", zap.NewNop())
	assert.Error(t, err)
}
