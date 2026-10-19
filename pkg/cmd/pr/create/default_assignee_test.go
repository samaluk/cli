package create

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/context"
	"github.com/cli/cli/v2/git"
	"github.com/cli/cli/v2/internal/browser"
	"github.com/cli/cli/v2/internal/config"
	fd "github.com/cli/cli/v2/internal/featuredetection"
	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/internal/prompter"
	"github.com/cli/cli/v2/internal/run"
	"github.com/cli/cli/v2/internal/telemetry"
	"github.com/cli/cli/v2/pkg/cmd/pr/shared"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/httpmock"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCmdCreate_assigneeProvided(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		provided  bool
		assignees []string
	}{
		{name: "omitted"},
		{name: "empty", args: []string{"--assignee="}, provided: true, assignees: []string{}},
		{name: "short flag", args: []string{"-a", "@me"}, provided: true, assignees: []string{"@me"}},
		{name: "repeated and comma separated", args: []string{"--assignee", "monalisa,hubot", "-a", "octocat"}, provided: true, assignees: []string{"monalisa", "hubot", "octocat"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ios, _, stdout, stderr := iostreams.Test()
			f := &cmdutil.Factory{IOStreams: ios, Config: func() (gh.Config, error) {
				return config.NewMockConfig(), nil
			}}
			var got *CreateOptions
			cmd := NewCmdCreate(f, &telemetry.InvocationRecorderSpy{}, func(opts *CreateOptions) error {
				got = opts
				return nil
			})
			cmd.SetArgs(append([]string{"--title", "title", "--body", ""}, tt.args...))
			cmd.SetOut(stderr)
			cmd.SetErr(stderr)
			_, err := cmd.ExecuteC()
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.provided, got.AssigneeProvided)
			assert.Equal(t, tt.assignees, got.Assignees)
			assert.Empty(t, stdout.String())
			assert.Empty(t, stderr.String())
		})
	}
}

func TestNewIssueState_defaultAssignee(t *testing.T) {
	tests := []struct {
		name         string
		config       string
		host         string
		explicit     []string
		provided     bool
		configErr    error
		resolveMe    bool
		resolveError bool
		want         []string
		wantErr      string
	}{
		{name: "unset"},
		{name: "global login", config: "pr_create_assignee: monalisa", want: []string{"monalisa"}},
		{name: "global self assignment", config: "pr_create_assignee: '@me'", resolveMe: true, want: []string{"active-viewer"}},
		{name: "target host self assignment", config: "pr_create_assignee: monalisa\nhosts:\n  example.com:\n    user: stored-user\n    pr_create_assignee: '@me'", host: "example.com", resolveMe: true, want: []string{"active-viewer"}},
		{name: "host override", config: "pr_create_assignee: '@me'\nhosts:\n  example.com:\n    pr_create_assignee: hubot", host: "example.com", want: []string{"hubot"}},
		{name: "host inherits global", config: "pr_create_assignee: monalisa", host: "example.com", want: []string{"monalisa"}},
		{name: "empty host masks global", config: "pr_create_assignee: '@me'\nhosts:\n  example.com:\n    pr_create_assignee: ''", host: "example.com"},
		{name: "explicit replaces default", config: "pr_create_assignee: '@me'", explicit: []string{"hubot"}, provided: true, want: []string{"hubot"}},
		{name: "explicit empty disables default", config: "pr_create_assignee: '@me'", provided: true},
		{name: "explicit skips config error", explicit: []string{"hubot"}, provided: true, configErr: errors.New("config failed"), want: []string{"hubot"}},
		{name: "empty skips config error", provided: true, configErr: errors.New("config failed")},
		{name: "config error", configErr: errors.New("config failed"), wantErr: "config failed"},
		{name: "self resolution error", config: "pr_create_assignee: '@me'", resolveMe: true, resolveError: true, wantErr: "failed resolving `@me` to your user handle: GraphQL: viewer unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := tt.host
			if host == "" {
				host = "github.com"
			}
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			if tt.resolveMe {
				response := `{"data":{"viewer":{"login":"active-viewer"}}}`
				if tt.resolveError {
					response = `{"errors":[{"message":"viewer unavailable"}]}`
				}
				reg.Register(httpmock.GraphQL(`query UserCurrent\b`), httpmock.StringResponse(response))
			}
			ctx := CreateContext{
				Client: api.NewClientFromHTTP(&http.Client{Transport: reg}),
				PRRefs: skipPushRefs{baseRepo: api.InitRepoHostname(&api.Repository{}, host)},
			}
			opts := CreateOptions{
				Config: func() (gh.Config, error) {
					return config.NewMockConfigFromString(tt.config), tt.configErr
				},
				TitleProvided: true, BodyProvided: true,
				Assignees: tt.explicit, AssigneeProvided: tt.provided,
			}
			state, err := NewIssueState(ctx, opts, true)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, append([]string(nil), state.Assignees...))
			for _, request := range reg.Requests {
				apiHost := host
				if host == "github.com" {
					apiHost = "api.github.com"
				}
				assert.Equal(t, apiHost, request.URL.Host)
			}
		})
	}
}

func TestNewCmdCreate_defaultAssigneeRepoOverride(t *testing.T) {
	for _, useEnv := range []bool{false, true} {
		name := "repo flag"
		if useEnv {
			name = "GH_REPO"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("GH_REPO", "")
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.StubRepoInfoResponse("OWNER", "REPO", "main")
			reg.Register(httpmock.GraphQL(`query UserCurrent\b`), httpmock.StringResponse(`{"data":{"viewer":{"login":"enterprise-user"}}}`))
			cs, teardown := run.Stub()
			defer teardown(t)
			cs.Register(`git config --get-regexp \^branch\\\..+\\\.\(remote\|merge\|pushremote\|gh-merge-base\)\$`, 0, "")
			ios, _, stdout, stderr := iostreams.Test()
			f := &cmdutil.Factory{
				IOStreams: ios,
				Config: func() (gh.Config, error) {
					return config.NewMockConfigFromString("pr_create_assignee: wrong-user\nhosts:\n  example.com:\n    pr_create_assignee: '@me'"), nil
				},
				HttpClient: func() (*http.Client, error) { return &http.Client{Transport: reg}, nil },
				GitClient:  &git.Client{GitPath: "some/path/git"},
				Remotes: func() (context.Remotes, error) {
					return context.Remotes{{Remote: &git.Remote{Name: "origin", Resolved: "base"}, Repo: ghrepo.New("OTHER", "REPO")}}, nil
				},
			}
			parent := &cobra.Command{Use: "pr"}
			cmdutil.EnableRepoOverride(parent, f)
			parent.AddCommand(NewCmdCreate(f, &telemetry.InvocationRecorderSpy{}, func(opts *CreateOptions) error {
				ctx, err := NewCreateContext(opts)
				require.NoError(t, err)
				state, err := NewIssueState(*ctx, *opts, false)
				require.NoError(t, err)
				assert.Equal(t, []string{"enterprise-user"}, state.Assignees)
				return nil
			}))
			args := []string{"create", "--head", "feature", "--title", "title", "--body", ""}
			if useEnv {
				t.Setenv("GH_REPO", "example.com/OWNER/REPO")
			} else {
				args = append(args, "-R", "example.com/OWNER/REPO")
			}
			parent.SetArgs(args)
			parent.SetOut(stderr)
			parent.SetErr(stderr)
			_, err := parent.ExecuteC()
			require.NoError(t, err)
			require.Len(t, reg.Requests, 2)
			for _, request := range reg.Requests {
				assert.Equal(t, "example.com", request.URL.Host)
			}
			assert.Empty(t, stdout.String())
			assert.Empty(t, stderr.String())
		})
	}
}

func Test_createRun_defaultAssignee(t *testing.T) {
	tests := []struct {
		name          string
		web           bool
		dryRun        bool
		tty           bool
		interactive   bool
		clearInPicker bool
		recoverEmpty  bool
		legacy        bool
		assignmentErr bool
		provided      bool
		explicit      []string
		wantAssignee  string
	}{
		{name: "noninteractive", wantAssignee: "monalisa"},
		{name: "explicit override", provided: true, explicit: []string{"hubot"}, wantAssignee: "hubot"},
		{name: "explicit opt out", provided: true},
		{name: "web", web: true, wantAssignee: "monalisa"},
		{name: "web opt out", web: true, provided: true},
		{name: "dry run plain", dryRun: true, wantAssignee: "monalisa"},
		{name: "dry run TTY", dryRun: true, tty: true, wantAssignee: "monalisa"},
		{name: "interactive", tty: true, interactive: true, wantAssignee: "monalisa"},
		{name: "picker clears default", tty: true, interactive: true, clearInPicker: true},
		{name: "recovery preserves empty assignees", tty: true, interactive: true, recoverEmpty: true},
		{name: "GHES metadata", legacy: true, wantAssignee: "monalisa"},
		{name: "assignment fails after creation", assignmentErr: true, wantAssignee: "monalisa"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.StubRepoInfoResponse("OWNER", "REPO", "main")
			if tt.legacy {
				reg.Register(httpmock.GraphQL(`query RepositoryAssignableUsers\b`), httpmock.StringResponse(`{"data":{"repository":{"assignableUsers":{"nodes":[{"id":"MONAID","login":"monalisa"}],"pageInfo":{"hasNextPage":false}}}}}`))
			}
			if !tt.web && !tt.dryRun {
				reg.Register(httpmock.GraphQL(`mutation PullRequestCreate\b`), httpmock.StringResponse(`{"data":{"createPullRequest":{"pullRequest":{"id":"PRID","url":"https://github.com/OWNER/REPO/pull/1"}}}}`))
				if tt.wantAssignee != "" {
					query := `mutation ReplaceActorsForAssignable\b`
					if tt.legacy {
						query = `mutation PullRequestCreateMetadata\b`
					}
					response := `{"data":{}}`
					if tt.assignmentErr {
						response = `{"errors":[{"message":"assignment denied"}]}`
					}
					reg.Register(httpmock.GraphQL(query), httpmock.GraphQLMutation(response, func(input map[string]any) {
						if tt.legacy {
							assert.Equal(t, []any{"MONAID"}, input["assigneeIds"])
						} else {
							assert.Equal(t, []any{tt.wantAssignee}, input["actorLogins"])
						}
					}))
				}
			}
			cs, teardown := run.Stub()
			defer teardown(t)
			cs.Register(`git config --get-regexp \^branch\\\..+\\\.\(remote\|merge\|pushremote\|gh-merge-base\)\$`, 0, "")
			if tt.interactive {
				cs.Register(`git( .+)? log( .+)? origin/main\.\.\.feature`, 0, "")
			}
			ios, _, stdout, stderr := iostreams.Test()
			ios.SetStdinTTY(tt.tty)
			ios.SetStdoutTTY(tt.tty)
			ios.SetStderrTTY(tt.tty)
			browse := &browser.Stub{}
			pm := &prompter.PrompterMock{}
			pm.InputFunc = func(prompt, _ string) (string, error) {
				assert.Equal(t, "Title (required)", prompt)
				return "title", nil
			}
			confirmations := 0
			pm.SelectFunc = func(prompt, _ string, options []string) (int, error) {
				assert.Equal(t, "What's next?", prompt)
				confirmations++
				if confirmations == 1 && !tt.recoverEmpty {
					assert.NotContains(t, options, "Continue in browser")
				}
				if tt.clearInPicker && confirmations == 1 {
					return prompter.IndexFor(options, "Add metadata")
				}
				return prompter.IndexFor(options, "Submit")
			}
			pm.MultiSelectFunc = func(prompt string, _ []string, options []string) ([]int, error) {
				assert.Equal(t, "What would you like to add?", prompt)
				return prompter.IndexesFor(options, "Assignees")
			}
			pm.MultiSelectWithSearchFunc = func(prompt, _ string, defaults, _ []string, _ func(string) prompter.MultiSelectSearchResult) ([]string, error) {
				assert.Equal(t, "Assignees", prompt)
				assert.Equal(t, []string{"monalisa"}, defaults)
				return []string{}, nil
			}
			opts := &CreateOptions{
				IO: ios, Browser: browse, Prompter: pm,
				HttpClient: func() (*http.Client, error) { return &http.Client{Transport: reg}, nil },
				Config:     func() (gh.Config, error) { return config.NewMockConfigFromString("pr_create_assignee: monalisa"), nil },
				GitClient:  &git.Client{GitPath: "some/path/git"},
				Remotes: func() (context.Remotes, error) {
					return context.Remotes{{Remote: &git.Remote{Name: "origin", Resolved: "base"}, Repo: ghrepo.New("OWNER", "REPO")}}, nil
				},
				Finder: shared.NewMockFinder("feature", nil, nil), Detector: &fd.EnabledDetectorMock{},
				HeadBranch: "feature", Title: "title", Body: "body", TitleProvided: !tt.interactive, BodyProvided: true,
				Assignees: tt.explicit, AssigneeProvided: tt.provided, WebMode: tt.web, DryRun: tt.dryRun,
			}
			if tt.legacy {
				opts.Detector = &fd.DisabledDetectorMock{}
			}
			if tt.recoverEmpty {
				opts.RecoverFile = filepath.Join(t.TempDir(), "recover.json")
				require.NoError(t, os.WriteFile(opts.RecoverFile, []byte(`{"Title":"title","Body":"body","Assignees":[]}`), 0600))
			}
			err := createRun(opts)
			if tt.assignmentErr {
				require.EqualError(t, err, "pull request update failed: GraphQL: assignment denied")
			} else {
				require.NoError(t, err)
			}
			switch {
			case tt.web:
				u, err := url.Parse(browse.BrowsedURL())
				require.NoError(t, err)
				assert.Equal(t, tt.wantAssignee, u.Query().Get("assignees"))
				assert.Empty(t, stdout.String())
			case tt.dryRun:
				if tt.tty {
					assert.Contains(t, stdout.String(), "Assignees: monalisa\n")
				} else {
					assert.Contains(t, stdout.String(), "assignees:\tmonalisa\n")
				}
			default:
				assert.Equal(t, "https://github.com/OWNER/REPO/pull/1\n", stdout.String())
			}
			if tt.tty {
				assert.Contains(t, stderr.String(), "pull request for feature into main in OWNER/REPO")
			} else {
				assert.Empty(t, stderr.String())
			}
		})
	}
}
