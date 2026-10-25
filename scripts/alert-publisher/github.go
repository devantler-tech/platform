package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
)

// These are reviewed public repository/App identities, not input from alerts.
const (
	repositoryOwner        = "devantler-tech"
	repositoryName         = "platform"
	repositoryID     int64 = 494599576
	publisherAppID   int64 = 4085939
	publisherAppSlug       = "provider-upjet-github"
)

type installationScope struct {
	appID        int64
	repositories []github.Repository
	permissions  github.InstallationPermissions
}

func validateScope(scope installationScope) error {
	if scope.appID != publisherAppID || len(scope.repositories) != 1 {
		return errQuarantined
	}
	r := scope.repositories[0]
	if r.GetID() != repositoryID || r.GetFullName() != repositoryOwner+"/"+repositoryName || r.Private == nil || r.GetPrivate() || r.GetOwner().GetLogin() != repositoryOwner {
		return errQuarantined
	}
	b, err := json.Marshal(scope.permissions)
	if err != nil {
		return errQuarantined
	}
	var grants map[string]string
	if json.Unmarshal(b, &grants) != nil || len(grants) != 2 || grants["issues"] != "write" || grants["metadata"] != "read" {
		return errQuarantined
	}
	return nil
}

type scopedTransport struct {
	installation *ghinstallation.Transport
	base         http.RoundTripper
	mu           sync.Mutex
}

type tokenMintFailure struct{ headers http.Header }

func (*tokenMintFailure) Error() string { return "alert token mint is unavailable" }

func (t *scopedTransport) token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	token, err := t.installation.Token(ctx)
	if err != nil {
		headers := http.Header{}
		var failure *ghinstallation.HTTPError
		if errors.As(err, &failure) && failure.Response != nil {
			for _, name := range []string{"Retry-After", "X-RateLimit-Reset"} {
				headers.Set(name, failure.Response.Header.Get(name))
			}
			if failure.Response.Body != nil {
				_ = failure.Response.Body.Close()
			}
			if failure.Response.StatusCode == 400 || failure.Response.StatusCode == 404 || failure.Response.StatusCode == 422 {
				return "", errQuarantined
			}
		}
		return "", &tokenMintFailure{headers: headers}
	}
	permissions, err := t.installation.Permissions()
	if err != nil {
		return "", errQuarantined
	}
	repos, err := t.installation.Repositories()
	if err != nil {
		return "", errQuarantined
	}
	if err = validateScope(installationScope{t.installation.AppID(), repos, permissions}); err != nil {
		return "", err
	}
	return token, nil
}

func (t *scopedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "api.github.com" || req.URL.User != nil || t.base == nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errQuarantined
	}
	token, err := t.token(req.Context())
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	// Send the same validated token, avoiding a second implicit refresh between
	// grant validation and the API request.
	request := req.Clone(req.Context())
	request.Header.Set("Authorization", "Bearer "+token)
	resp, err := t.base.RoundTrip(request)
	if resp != nil && resp.Body != nil {
		resp.Body = &boundedBody{Reader: io.LimitReader(resp.Body, 8*1024*1024), closer: resp.Body}
	}
	return resp, err
}

type boundedBody struct {
	io.Reader
	closer io.Closer
}

func (b *boundedBody) Close() error { return b.closer.Close() }

type githubRemote struct {
	client   *github.Client
	now      func() time.Time
	verified bool
}

func newGitHubRemote(installationID int64, key []byte) (*githubRemote, error) {
	if installationID <= 0 || len(key) == 0 || len(key) > 64*1024 {
		return nil, errQuarantined
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errQuarantined
	}
	base := defaultTransport.Clone()
	transport, err := ghinstallation.New(base, publisherAppID, installationID, key)
	if err != nil {
		return nil, errQuarantined
	}
	transport.InstallationTokenOptions = &github.InstallationTokenOptions{
		RepositoryIDs: []int64{repositoryID},
		Permissions:   &github.InstallationPermissions{Issues: github.Ptr("write")},
	}
	transport.Client = &http.Client{Transport: base, Timeout: 10 * time.Second, CheckRedirect: noRedirect}
	client, err := github.NewClient(github.WithHTTPClient(&http.Client{Transport: &scopedTransport{installation: transport, base: base}, Timeout: 15 * time.Second, CheckRedirect: noRedirect}))
	if err != nil {
		return nil, errQuarantined
	}
	return &githubRemote{client: client, now: time.Now}, nil
}
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (a *githubRemote) verify(ctx context.Context) error {
	a.verified = false
	app, _, err := a.client.Apps.Get(ctx, publisherAppSlug)
	if err != nil {
		return a.apiError(err, false)
	}
	if app.GetID() != publisherAppID || app.GetSlug() != publisherAppSlug || app.GetOwner().GetLogin() != repositoryOwner {
		return errQuarantined
	}
	r, _, err := a.client.Repositories.Get(ctx, repositoryOwner, repositoryName)
	if err != nil {
		return a.apiError(err, false)
	}
	if r.GetID() != repositoryID || r.GetFullName() != repositoryOwner+"/"+repositoryName || r.Private == nil || r.Archived == nil || r.HasIssues == nil || r.GetPrivate() || r.GetArchived() || !r.GetHasIssues() || r.GetOwner().GetLogin() != repositoryOwner {
		return errQuarantined
	}
	a.verified = true
	return nil
}
func trustedIssue(i *github.Issue) bool {
	return i != nil && !i.IsPullRequest() && i.GetNumber() > 0 && i.GetUser().GetID() > 0 && i.GetUser().GetLogin() == publisherAppSlug+"[bot]" && i.GetUser().GetType() == "Bot"
}
func trustedComment(c *github.IssueComment) bool {
	return c != nil && c.GetID() > 0 && c.GetUser().GetID() > 0 && c.GetUser().GetLogin() == publisherAppSlug+"[bot]" && c.GetUser().GetType() == "Bot"
}
func (a *githubRemote) isOpen(ctx context.Context, number int) (bool, error) {
	i, _, err := a.client.Issues.Get(ctx, repositoryOwner, repositoryName, number)
	if err != nil {
		return false, a.apiError(err, false)
	}
	if !trustedIssue(i) || i.GetNumber() != number || (i.GetState() != "open" && i.GetState() != "closed") {
		return false, errQuarantined
	}
	return i.GetState() == "open", nil
}

func (a *githubRemote) find(ctx context.Context, op operation) (receipt, error) {
	var result receipt
	since := op.CreatedAt.Add(-5 * time.Minute)
	for page := 1; page <= 20; page++ {
		var response *github.Response
		var err error
		if op.Issue == 0 {
			var issues []*github.Issue
			issues, response, err = a.client.Issues.ListByRepo(ctx, repositoryOwner, repositoryName, &github.IssueListByRepoOptions{State: "all", Sort: "created", Direction: "desc", Since: since, ListOptions: github.ListOptions{Page: page, PerPage: 50}})
			if err == nil {
				for _, i := range issues {
					if trustedIssue(i) && strings.Contains(i.GetBody(), op.marker()) {
						if result.Issue != 0 || i.GetBody() != op.body() {
							return receipt{}, errQuarantined
						}
						result = receipt{Issue: i.GetNumber()}
					}
				}
			}
		} else {
			var comments []*github.IssueComment
			comments, response, err = a.client.Issues.ListComments(ctx, repositoryOwner, repositoryName, op.Issue, &github.IssueListCommentsOptions{Since: &since, ListOptions: github.ListOptions{Page: page, PerPage: 50}})
			if err == nil {
				for _, c := range comments {
					if trustedComment(c) && strings.Contains(c.GetBody(), op.marker()) {
						if result.Issue != 0 || c.GetBody() != op.body() {
							return receipt{}, errQuarantined
						}
						result = receipt{Issue: op.Issue, Comment: int(c.GetID())}
					}
				}
			}
		}
		if err != nil {
			return receipt{}, a.apiError(err, false)
		}
		if response == nil {
			return receipt{}, errQuarantined
		}
		if response.NextPage == 0 {
			return result, nil
		}
		if response.NextPage != page+1 {
			return receipt{}, errQuarantined
		}
	}
	return receipt{}, errQuarantined
}
func (a *githubRemote) write(ctx context.Context, op operation) error {
	// The engine verifies identity before committing Sent=true. No pre-write
	// GET belongs after that commit: its failure cannot make an unsent POST
	// ambiguous. Consume the preceding verification once.
	if !a.verified {
		return errQuarantined
	}
	a.verified = false
	var response *github.Response
	var err error
	if op.Issue == 0 {
		_, response, err = a.client.Issues.Create(ctx, repositoryOwner, repositoryName, &github.IssueRequest{Title: github.Ptr(routes[op.Route]), Body: github.Ptr(op.body()), Type: github.Ptr("Bug")})
	} else {
		_, response, err = a.client.Issues.CreateComment(ctx, repositoryOwner, repositoryName, op.Issue, &github.IssueComment{Body: github.Ptr(op.body())})
	}
	if err != nil {
		return a.apiError(err, true)
	}
	if response == nil || response.StatusCode != http.StatusCreated {
		return errAmbiguous
	}
	return nil
}
func (a *githubRemote) apiError(err error, writing bool) error {
	if errors.Is(err, errQuarantined) {
		return errQuarantined
	}
	var mint *tokenMintFailure
	if errors.As(err, &mint) {
		return &retryError{after: retryDeadline(mint.headers, a.now())}
	}
	var response *github.ErrorResponse
	var rate *github.RateLimitError
	var abuse *github.AbuseRateLimitError
	status := 0
	var headers http.Header
	if errors.As(err, &response) && response.Response != nil {
		status = response.Response.StatusCode
		headers = response.Response.Header
	}
	if errors.As(err, &rate) {
		status = 403
		if rate.Response != nil {
			headers = rate.Response.Header
		}
	}
	if errors.As(err, &abuse) {
		status = 429
		if abuse.Response != nil {
			headers = abuse.Response.Header
		}
	}
	if status == 401 || status == 403 || status == 429 {
		return &retryError{after: retryDeadline(headers, a.now())}
	}
	if status > 0 && status < 500 {
		return errQuarantined
	}
	if writing {
		return errAmbiguous
	}
	return errors.New("alert GitHub read is unavailable")
}

func retryDeadline(headers http.Header, now time.Time) time.Time {
	after := now.Add(5 * time.Minute)
	if seconds, err := strconv.Atoi(headers.Get("Retry-After")); err == nil && seconds > 0 {
		after = now.Add(time.Duration(min(seconds, 3600)) * time.Second)
	} else if date, err := http.ParseTime(headers.Get("Retry-After")); err == nil && date.After(now) {
		after = date
	}
	if reset, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		candidate := time.Unix(reset, 0)
		if candidate.After(after) {
			after = candidate
		}
	}
	if after.After(now.Add(time.Hour)) {
		after = now.Add(time.Hour)
	}
	return after
}
