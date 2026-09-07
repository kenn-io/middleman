package github

import (
	"time"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/platform"
)

type RateLimitSnapshot struct {
	Core    *platform.Rate
	GraphQL *platform.Rate
}

func RateFromGitHub(rate gh.Rate) platform.Rate {
	return platform.Rate{Limit: rate.Limit, Remaining: rate.Remaining, Reset: rate.Reset.Time}
}

func RateFromHeaders(limit, remaining int, reset time.Time) platform.Rate {
	return platform.Rate{Limit: limit, Remaining: remaining, Reset: reset}
}
