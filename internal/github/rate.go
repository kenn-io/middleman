package github

import (
	"strings"

	"go.kenn.io/forge/platform"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/ratelimit"
)

const RateReserveBuffer = ratelimit.RateReserveBuffer

type Rate = platform.Rate
type RateTracker = ratelimit.RateTracker

func NewRateTracker(
	database *db.DB, platformHost, ratePrincipal, apiType string,
) *RateTracker {
	return ratelimit.NewPlatformRateTracker(
		database, "github", platformHost, ratePrincipal, apiType,
	)
}

func NewPlatformRateTracker(
	database *db.DB,
	platformName, platformHost, ratePrincipal, apiType string,
) *RateTracker {
	return ratelimit.NewPlatformRateTracker(
		database, platformName, platformHost, ratePrincipal, apiType,
	)
}

func RateBucketKey(platformName, platformHost, ratePrincipal string) string {
	return ratelimit.RateBucketKey(platformName, platformHost, ratePrincipal)
}

// RateStatusKey returns the public API key for one provider credential's rate
// and local-ceiling status. Internal bucket keys use NUL separators for
// unambiguous map partitioning; API keys remain printable and stable.
func RateStatusKey(platformName, platformHost, ratePrincipal string) string {
	if ratePrincipal == "" || ratePrincipal == "host" {
		return RateBucketKey(platformName, platformHost, "host")
	}
	return strings.Join([]string{platformName, platformHost, ratePrincipal}, ":")
}
