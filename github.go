package gogithub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/shurcooL/githubv4"
	"github.com/shurcooL/graphql"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"
)

type GitHub interface {
	// CreatePullRequest creates a PR of your current branch.  It assumes there is a remote branch with the
	// exact same name.  It will fail if you're already on master or main.
	CreatePullRequest(ctx context.Context, remoteRepositoryId graphql.ID, baseRefName string, remoteRefName string, title string, body string) (int64, error)
	// RepositoryInfo returns special information about a remote repository
	RepositoryInfo(ctx context.Context, owner string, name string) (*RepositoryInfo, error)
	// FindPRForBranch returns the PR for this branch
	FindPRForBranch(ctx context.Context, owner string, name string, branch string) (int64, error)
	// Self returns the current user
	Self(ctx context.Context) (string, error)
	// AcceptPullRequest approves a PR
	AcceptPullRequest(ctx context.Context, approvalmessage string, owner string, name string, number int64) error
	// MergePullRequest merges in a PR and closes it, but only if it's approved
	MergePullRequest(ctx context.Context, owner string, name string, number int64) error
	// EnablePullRequestAutoMerge enables auto-merge for the specified pull request
	EnablePullRequestAutoMerge(ctx context.Context, owner string, name string, number int64) error
	// FindPullRequest returns basic information for the specified pull request
	FindPullRequest(ctx context.Context, owner string, name string, number int64) (*PullRequest, error)
	// AddPRComment adds a comment to the specified pull request
	AddPRComment(ctx context.Context, owner string, name string, number int64, body string) error
	// FindPullRequestOid returns the OID of the PR
	FindPullRequestOid(ctx context.Context, owner string, name string, number int64) (githubv4.ID, error)
	GetAccessToken(ctx context.Context) (string, error)
	TriggerWorkflow(ctx context.Context, owner string, repo string, workflow_id string, ref string, inputs map[string]string) error
}

type RepositoryInfo struct {
	Repository struct {
		ID               githubv4.ID
		DefaultBranchRef struct {
			Name githubv4.String
			ID   githubv4.ID
		}
	} `graphql:"repository(owner: $owner, name: $name)"`
}

type PullRequest struct {
	ID githubv4.ID
	// Number identifies the pull request number.
	Number int64
	// BaseRefName identifies the name of the base Ref associated with the pull request, even if the ref has been deleted.
	BaseRefName string
	// BaseRefOid identifies the oid of the base ref associated with the pull request, even if the ref has been deleted.
	BaseRefOid githubv4.ID
	// HeadRefName identifies the name of the head Ref associated with the pull request, even if the ref has been deleted.
	HeadRefName string
	// HeadRefOid identifies the oid of the head ref associated with the pull request, even if the ref has been deleted.
	HeadRefOid githubv4.ID
	// Body as Markdown.
	Body string
	// Closed is true if the pull request is closed.
	State PullRequestState
}

type PullRequestState string

const (
	PullRequstClosed PullRequestState = "CLOSED"
	PullRequstMerged PullRequestState = "MERGED"
	PullRequstOpen   PullRequestState = "OPEN"
)

type createPullRequest struct {
	CreatePullRequest struct {
		// Note: This is unused, but the library requires at least something to be read for the mutation to happen
		ClientMutationID githubv4.ID
		PullRequest      struct {
			Number githubv4.Int
		}
	} `graphql:"createPullRequest(input: $input)"`
}

type GithubGraphqlAPI struct {
	ClientV4      *githubv4.Client
	Logger        *zap.Logger
	tokenFunction func(ctx context.Context) (string, error)
	findPrCache   ExpireCache[findPrKey, findPrValue]
	HttpClient    *http.Client
}

type triggerWorkflowBody struct {
	Ref    string            `json:"ref"`
	Inputs map[string]string `json:"inputs"`
}

func (g *GithubGraphqlAPI) TriggerWorkflow(ctx context.Context, owner string, repo string, workflow_id string, ref string, inputs map[string]string) error {
	g.Logger.Debug("TriggerWorkflow", zap.String("owner", owner), zap.String("repo", repo), zap.String("workflow_id", workflow_id), zap.String("ref", ref), zap.Any("inputs", inputs))
	defer g.Logger.Debug("Done TriggerWorkflow")
	token, err := g.GetAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}
	body := triggerWorkflowBody{
		Ref:    ref,
		Inputs: inputs,
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/workflows/%s/dispatches", owner, repo, workflow_id)
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to encode request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encodedBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	resp, err := g.HttpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("failed to trigger workflow: %s", resp.Status)
	}
	return nil
}

type findPrKey struct {
	owner  string
	name   string
	branch string
}

type findPrValue struct {
	number int64
}

func (g *GithubGraphqlAPI) GetAccessToken(ctx context.Context) (string, error) {
	return g.tokenFunction(ctx)
}

func (g *GithubGraphqlAPI) FindPullRequestOid(ctx context.Context, owner string, name string, number int64) (githubv4.ID, error) {
	g.Logger.Debug("FindPullRequestOid", zap.String("owner", owner), zap.String("name", name), zap.Int64("number", number))
	defer g.Logger.Debug("Done FindPullRequestOid")
	var query struct {
		Repository struct {
			PullRequest struct {
				ID githubv4.ID
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	variables := map[string]interface{}{
		"owner":  githubv4.String(owner),
		"name":   githubv4.String(name),
		"number": githubv4.Int(number),
	}
	err := g.ClientV4.Query(ctx, &query, variables)
	if err != nil {
		return 0, fmt.Errorf("failed to query for PRs: %w", err)
	}
	if query.Repository.PullRequest.ID == 0 {
		return 0, fmt.Errorf("failed to find PR %d", number)
	}
	return query.Repository.PullRequest.ID, nil
}

func (g *GithubGraphqlAPI) AcceptPullRequest(ctx context.Context, approvalmessage string, owner string, name string, number int64) error {
	defer g.findPrCache.Clear()
	prid, err := g.FindPullRequestOid(ctx, owner, name, number)
	if err != nil {
		return fmt.Errorf("failed to find PR: %w", err)
	}
	g.Logger.Debug("AcceptPullRequest", zap.String("owner", owner), zap.String("name", name), zap.Int64("number", number), zap.Any("prid", prid))
	defer g.Logger.Debug("Done AcceptPullRequest")
	event := githubv4.PullRequestReviewEventApprove
	body := githubv4.String(approvalmessage)
	var ret struct {
		AddPullRequestReview struct {
			PullRequestReview struct {
				ID githubv4.ID
			}
		} `graphql:"addPullRequestReview(input: $input)"`
	}
	if err := g.ClientV4.Mutate(ctx, &ret, githubv4.AddPullRequestReviewInput{
		PullRequestID: prid,
		Body:          &body,
		Event:         &event,
	}, nil); err != nil {
		return fmt.Errorf("uanble to add PR review: %w", err)
	}
	return nil
}

func (g *GithubGraphqlAPI) MergePullRequest(ctx context.Context, owner string, name string, number int64) error {
	defer g.findPrCache.Clear()
	prid, err := g.FindPullRequestOid(ctx, owner, name, number)
	if err != nil {
		return fmt.Errorf("failed to find PR: %w", err)
	}
	g.Logger.Debug("MergePullRequest", zap.String("owner", owner), zap.String("name", name), zap.Int64("number", number), zap.Any("prid", prid))
	defer g.Logger.Debug("Done MergePullRequest")
	var ret struct {
		MergePullRequest struct {
			PullRequest struct {
				ID githubv4.ID
			}
		} `graphql:"mergePullRequest(input: $input)"`
	}
	mergeMethod := githubv4.PullRequestMergeMethodSquash
	if err := g.ClientV4.Mutate(ctx, &ret, githubv4.MergePullRequestInput{
		PullRequestID: prid,
		MergeMethod:   &mergeMethod,
	}, nil); err != nil {
		return fmt.Errorf("uanble to add PR review: %w", err)
	}
	return nil
}

type GraphQLPRQueryNode struct {
	Number githubv4.Int
}

func (g *GithubGraphqlAPI) FindPRForBranch(ctx context.Context, owner string, name string, branch string) (int64, error) {
	g.Logger.Debug("FindPRForBranch", zap.String("owner", owner), zap.String("name", name), zap.String("branch", branch))
	defer g.Logger.Debug("Done FindPRForBranch")
	cacheKey := findPrKey{
		owner:  owner,
		name:   name,
		branch: branch,
	}
	prNum, exists := g.findPrCache.Get(cacheKey)
	if exists {
		g.Logger.Debug("pr cached value", zap.Int64("prNum", prNum.number))
		return prNum.number, nil
	}

	var query struct {
		Repository struct {
			PullRequests struct {
				Nodes []GraphQLPRQueryNode `graphql:"nodes"`
			} `graphql:"pullRequests(states: [OPEN], first: 10, headRefName: $branch)"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	variables := map[string]interface{}{
		"owner":  githubv4.String(owner),
		"name":   githubv4.String(name),
		"branch": githubv4.String(branch),
	}
	err := g.ClientV4.Query(ctx, &query, variables)
	if err != nil {
		return 0, fmt.Errorf("failed to query for PRs: %w", err)
	}
	if len(query.Repository.PullRequests.Nodes) == 0 {
		g.Logger.Debug("No PRs found")
		g.findPrCache.Set(cacheKey, findPrValue{number: int64(0)})
		return 0, nil
	}
	if len(query.Repository.PullRequests.Nodes) > 1 {
		return 0, fmt.Errorf("found multiple PRs for branch %s", branch)
	}
	pr := query.Repository.PullRequests.Nodes[0]
	g.findPrCache.Set(cacheKey, findPrValue{number: int64(pr.Number)})
	return int64(pr.Number), nil
}

func (g *GithubGraphqlAPI) EnablePullRequestAutoMerge(ctx context.Context, owner string, name string, number int64) error {
	prid, err := g.FindPullRequestOid(ctx, owner, name, number)
	if err != nil {
		return fmt.Errorf("failed to find PR: %w", err)
	}
	g.Logger.Debug("EnablePullRequestAutoMerge", zap.String("owner", owner), zap.String("name", name), zap.Int64("number", number), zap.Any("prid", prid))
	defer g.Logger.Debug("Done EnablePullRequestAutoMerge")
	var ret struct {
		AutoMergRequest struct {
			PullRequest struct {
				ID githubv4.ID
			}
		} `graphql:"enablePullRequestAutoMerge(input: $input)"`
	}
	mergeMethod := githubv4.PullRequestMergeMethodSquash
	if err := g.ClientV4.Mutate(ctx, &ret, githubv4.EnablePullRequestAutoMergeInput{
		PullRequestID: prid,
		MergeMethod:   &mergeMethod,
	}, nil); err != nil {
		return fmt.Errorf("uanble to enable PR auto-merge: %w", err)
	}
	return nil
}

func (g *GithubGraphqlAPI) FindPullRequest(ctx context.Context, owner string, name string, number int64) (*PullRequest, error) {
	g.Logger.Debug("FindPullRequest", zap.String("owner", owner), zap.String("name", name), zap.Int64("number", number))
	defer g.Logger.Debug("Done FindPullRequest")
	var query struct {
		Repository struct {
			PullRequest PullRequest `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	variables := map[string]interface{}{
		"owner":  githubv4.String(owner),
		"name":   githubv4.String(name),
		"number": githubv4.Int(number),
	}
	err := g.ClientV4.Query(ctx, &query, variables)
	if err != nil {
		return nil, fmt.Errorf("failed to query for PRs: %w", err)
	}
	if query.Repository.PullRequest.ID == 0 {
		return nil, fmt.Errorf("failed to find PR %d", number)
	}
	return &query.Repository.PullRequest, nil
}

func (g *GithubGraphqlAPI) AddPRComment(ctx context.Context, owner string, name string, number int64, body string) error {
	prid, err := g.FindPullRequestOid(ctx, owner, name, number)
	if err != nil {
		return fmt.Errorf("failed to find PR: %w", err)
	}
	g.Logger.Debug("AddPRComment", zap.String("owner", owner), zap.String("name", name), zap.Int64("number", number), zap.Any("prid", prid))
	defer g.Logger.Debug("Done AddPRComment")
	var ret struct {
		AddCommentRequest struct {
			ClientMutationId githubv4.String
		} `graphql:"addComment(input: $input)"`
	}
	if err := g.ClientV4.Mutate(ctx, &ret, githubv4.AddCommentInput{
		SubjectID: prid,
		Body:      githubv4.String(body),
	}, nil); err != nil {
		return fmt.Errorf("failed to add comment: %w", err)
	}
	return nil
}

type NewGQLClientConfig struct {
	Rt             http.RoundTripper
	AppID          int64
	InstallationID int64
	PEMKeyLoc      string
	Token          string
	PEMKey         string
	CacheTTL       time.Duration
}

var DefaultGQLClientConfig = NewGQLClientConfig{
	Rt:             http.DefaultTransport,
	AppID:          intFromOsEnv("GITHUB_APP_ID"),
	InstallationID: intFromOsEnv("GITHUB_INSTALLATION_ID"),
	PEMKeyLoc:      os.Getenv("GITHUB_PEM_KEY_LOC"),
	PEMKey:         os.Getenv("GITHUB_PEM_KEY"),
	Token:          os.Getenv("GITHUB_TOKEN"),
	CacheTTL:       time.Minute,
}

func intFromOsEnv(s string) int64 {
	v := os.Getenv(s)
	if v == "" {
		return 0
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return i
}

func createGraphqlAPI(gql *githubv4.Client, httpClient *http.Client, logger *zap.Logger, cacheTtl time.Duration, tokenFunction func(context.Context) (string, error)) *GithubGraphqlAPI {
	return &GithubGraphqlAPI{
		HttpClient:    httpClient,
		ClientV4:      gql,
		Logger:        logger,
		tokenFunction: tokenFunction,
		findPrCache: ExpireCache[findPrKey, findPrValue]{
			DefaultExpiry: cacheTtl,
		},
	}
}

func clientFromToken(_ context.Context, logger *zap.Logger, token string, cacheTtl time.Duration) (GitHub, error) {
	src := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	httpClient := oauth2.NewClient(context.Background(), src)
	httpClient.Transport = DebugLogTransport(httpClient.Transport, logger)
	gql := githubv4.NewClient(httpClient)
	return createGraphqlAPI(gql, httpClient, logger, cacheTtl, func(_ context.Context) (string, error) {
		return token, nil
	}), nil
}

func clientFromPEM(ctx context.Context, logger *zap.Logger, baseRoundTripper http.RoundTripper, appID int64, installID int64, pemLoc string, pemKey string, cacheTtl time.Duration) (GitHub, error) {
	if baseRoundTripper == nil {
		baseRoundTripper = http.DefaultTransport
	}
	var trans *ghinstallation.Transport
	var err error
	if pemKey != "" {
		trans, err = ghinstallation.New(baseRoundTripper, appID, installID, []byte(pemKey))
	} else {
		trans, err = ghinstallation.NewKeyFromFile(baseRoundTripper, appID, installID, pemLoc)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to find key file: %w", err)
	}
	_, err = trans.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to validate token: %w", err)
	}
	client := &http.Client{Transport: DebugLogTransport(trans, logger)}
	gql := githubv4.NewClient(client)
	return createGraphqlAPI(gql, client, logger, cacheTtl, trans.Token), nil
}

func tokenFromGithubCLI() string {
	s, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	configPath := filepath.Join(s, ".config", "gh", "hosts.yml")
	b, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var out map[string]configFileAuths
	if err := yaml.Unmarshal(b, &out); err != nil {
		return ""
	}
	return tokenForAny(out, "github.com", "Github.com")
}

func tokenForAny(m map[string]configFileAuths, hosts ...string) string {
	for _, host := range hosts {
		if auth, exists := m[host]; exists {
			return auth.Token
		}
	}
	return ""
}

type configFileAuths struct {
	Token string `yaml:"oauth_token"`
}

// NewGQLClient generates a new GraphQL github client
func NewGQLClient(ctx context.Context, logger *zap.Logger, cfg *NewGQLClientConfig) (GitHub, error) {
	cfg = mergeGithubConfigs(cfg, &DefaultGQLClientConfig)
	if cfg != nil && cfg.Token != "" {
		return clientFromToken(ctx, logger, cfg.Token, cfg.CacheTTL)
	}
	if cfg != nil && (cfg.PEMKeyLoc != "" || cfg.PEMKey != "") {
		return clientFromPEM(ctx, logger, cfg.Rt, cfg.AppID, cfg.InstallationID, cfg.PEMKeyLoc, cfg.PEMKey, cfg.CacheTTL)
	}
	if token := tokenFromGithubCLI(); token != "" {
		return clientFromToken(ctx, logger, token, cfg.CacheTTL)
	}
	return nil, fmt.Errorf("no token provided: I need either GITHUB_TOKEN env, existing auth via the `gh` CLI, or a PEM key")
}

func mergeGithubConfigs(cfg *NewGQLClientConfig, config *NewGQLClientConfig) *NewGQLClientConfig {
	if cfg == nil {
		return config
	}
	ret := *cfg
	if ret.Rt == nil {
		ret.Rt = config.Rt
	}
	if ret.AppID == 0 {
		ret.AppID = config.AppID
	}
	if ret.InstallationID == 0 {
		ret.InstallationID = config.InstallationID
	}
	if ret.PEMKeyLoc == "" {
		ret.PEMKeyLoc = config.PEMKeyLoc
	}
	if ret.Token == "" {
		ret.Token = config.Token
	}
	return &ret
}

func (g *GithubGraphqlAPI) Self(ctx context.Context) (string, error) {
	g.Logger.Debug("fetching self")
	defer g.Logger.Debug("done fetching self")
	var q struct {
		Viewer struct {
			Login githubv4.String
			ID    githubv4.ID
		}
	}
	if err := g.ClientV4.Query(ctx, &q, nil); err != nil {
		return "", fmt.Errorf("unable to run graphql query self: %w", err)
	}
	return string(q.Viewer.Login), nil
}

func (g *GithubGraphqlAPI) CreatePullRequest(ctx context.Context, remoteRepositoryId graphql.ID, baseRefName string, remoteRefName string, title string, body string) (int64, error) {
	defer g.findPrCache.Clear()
	g.Logger.Debug("creating pull request", zap.Any("remoteRepositoryId", remoteRepositoryId), zap.String("baseRefName", baseRefName), zap.String("remoteRefName", remoteRefName), zap.String("title", title), zap.String("body", body))
	defer g.Logger.Debug("done creating pull request")
	var ret createPullRequest
	if err := g.ClientV4.Mutate(ctx, &ret, githubv4.CreatePullRequestInput{
		RepositoryID: remoteRepositoryId,
		BaseRefName:  githubv4.String(baseRefName),
		HeadRefName:  githubv4.String(remoteRefName),
		Title:        githubv4.String(title),
		Body:         githubv4.NewString(githubv4.String(body)),
	}, nil); err != nil {
		return 0, fmt.Errorf("failed to create pull request: %w", err)
	}
	return int64(ret.CreatePullRequest.PullRequest.Number), nil
}

func (g *GithubGraphqlAPI) RepositoryInfo(ctx context.Context, owner string, name string) (*RepositoryInfo, error) {
	g.Logger.Debug("fetching repository info", zap.String("owner", owner), zap.String("name", name))
	defer g.Logger.Debug("done fetching repository info")
	var repoInfo RepositoryInfo
	if err := g.ClientV4.Query(ctx, &repoInfo, map[string]interface{}{
		"owner": githubv4.String(owner),
		"name":  githubv4.String(name),
	}); err != nil {
		return nil, fmt.Errorf("unable to query graphql for repository info: %w", err)
	}
	return &repoInfo, nil
}

var _ GitHub = &GithubGraphqlAPI{}
