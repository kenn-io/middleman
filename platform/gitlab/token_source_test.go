package gitlab

import (
	"context"

	"go.kenn.io/forge/internal/testutil/tokenauthtest"
	"go.kenn.io/forge/internal/tokenauth"
)

var testTokenSource = tokenauthtest.Source

type mutableTestTokenSource struct {
	token       string
	invalidated int
}

func newMutableTestTokenSource(token string) *mutableTestTokenSource {
	return &mutableTestTokenSource{token: token}
}

func (s *mutableTestTokenSource) Token(context.Context) (string, error) {
	return s.token, nil
}

func (s *mutableTestTokenSource) Invalidate(string) {
	s.invalidated++
}

func (s *mutableTestTokenSource) Descriptor() tokenauth.Descriptor {
	return tokenauth.Descriptor{Key: tokenauth.Key{Platform: "test", Host: "test"}}
}
