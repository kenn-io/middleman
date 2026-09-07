package gitealike

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRateFromHeadersRequiresCompleteProviderTuple(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	reset := time.Now().UTC().Add(30 * time.Minute).Unix()
	complete := http.Header{
		"X-Ratelimit-Limit":     []string{"5000"},
		"X-Ratelimit-Remaining": []string{"4999"},
		"X-Ratelimit-Reset":     []string{strconv.FormatInt(reset, 10)},
	}

	rate, ok := RateFromHeaders(complete)
	require.True(ok)
	assert.Equal(5000, rate.Limit)
	assert.Equal(4999, rate.Remaining)
	assert.Equal(reset, rate.Reset.Unix())

	complete.Del("X-Ratelimit-Reset")
	_, ok = RateFromHeaders(complete)
	assert.False(ok)
}
