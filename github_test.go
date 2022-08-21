package gogithub

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"testing"
)

func newClientOrSkip(t *testing.T) GitHub {
	logger := zaptest.NewLogger(t)
	gh, err := NewGQLClient(context.Background(), logger, nil)
	if err != nil {
		t.Skipf("skipping test: %v", err)
	}
	return gh
}

func TestGithubGraphQLAPI_GetAccessToken(t *testing.T) {
	gh := newClientOrSkip(t)
	tok, err := gh.GetAccessToken(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, tok)
}

func TestGithubGraphQLAPI_RepositoryInfo(t *testing.T) {
	gh := newClientOrSkip(t)
	repo, err := gh.RepositoryInfo(context.Background(), "cresta", "gogithub")
	require.NoError(t, err)
	require.Equal(t, "main", string(repo.Repository.DefaultBranchRef.Name))
}

func TestGithubGraphQLAPI_Self(t *testing.T) {
	gh := newClientOrSkip(t)
	self, err := gh.Self(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, self)
}
