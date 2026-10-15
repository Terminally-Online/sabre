package release

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	owner = "terminally-online"
	repo  = "sabre"
	token = "release-gate-token"
)

var (
	merged = strings.Repeat("a", 40)
	head   = strings.Repeat("b", 40)
	pushed = strings.Repeat("c", 40)

	required = []string{
		"verify / lint (pull_request)",
		"verify / test (pull_request)",
		"verify / build (pull_request)",
		"verify / security (pull_request)",
	}
)

type pull struct {
	Number         int    `json:"number"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Base           string `json:"-"`
	Head           string `json:"-"`
}

type protection struct {
	EnableStatusCheck   bool     `json:"enable_status_check"`
	StatusCheckContexts []string `json:"status_check_contexts"`
}

type status struct {
	Context string `json:"context"`
	Status  string `json:"status"`
}

type forge struct {
	mu            sync.Mutex
	defaultBranch string
	pulls         map[string]pull
	protections   map[string]protection
	statuses      map[string][]status
}

func passing() []status {
	out := make([]status, 0, len(required))
	for _, c := range required {
		out = append(out, status{Context: c, Status: "success"})
	}
	return out
}

func with(statuses []status, context, state string) []status {
	out := make([]status, len(statuses))
	for i, s := range statuses {
		if strings.Contains(s.Context, context) {
			s.Status = state
		}
		out[i] = s
	}
	return out
}

func newForge() *forge {
	return &forge{
		defaultBranch: "master",
		pulls:         map[string]pull{merged: {Number: 28, Merged: true, MergeCommitSHA: merged, Base: "master", Head: head}},
		protections:   map[string]protection{"master": {EnableStatusCheck: true, StatusCheckContexts: append([]string(nil), required...)}},
		statuses:      map[string][]status{head: passing()},
	}
}

var (
	pullPath       = regexp.MustCompile(`^/api/v1/repos/` + owner + `/` + repo + `/commits/([0-9a-f]{40})/pull$`)
	statusPath     = regexp.MustCompile(`^/api/v1/repos/` + owner + `/` + repo + `/commits/([0-9a-f]{40})/status$`)
	protectionPath = regexp.MustCompile(`^/api/v1/repos/` + owner + `/` + repo + `/branch_protections/(.+)$`)
)

func (f *forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	send := func(code int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
	missing := map[string]string{"message": "The target couldn't be found."}
	if r.Header.Get("Authorization") != "token "+token {
		send(http.StatusUnauthorized, map[string]string{"message": "unauthorized"})
		return
	}
	if r.Method != http.MethodGet {
		send(http.StatusMethodNotAllowed, map[string]string{"message": "read only"})
		return
	}
	p := r.URL.Path
	if p == "/api/v1/repos/"+owner+"/"+repo {
		send(http.StatusOK, map[string]string{"full_name": owner + "/" + repo, "default_branch": f.defaultBranch})
		return
	}
	if m := pullPath.FindStringSubmatch(p); m != nil {
		pr, ok := f.pulls[m[1]]
		if !ok {
			send(http.StatusNotFound, missing)
			return
		}
		send(http.StatusOK, map[string]any{
			"number":           pr.Number,
			"merged":           pr.Merged,
			"merge_commit_sha": pr.MergeCommitSHA,
			"base":             map[string]string{"ref": pr.Base},
			"head":             map[string]string{"sha": pr.Head},
		})
		return
	}
	if m := protectionPath.FindStringSubmatch(p); m != nil {
		rule, ok := f.protections[m[1]]
		if !ok {
			send(http.StatusNotFound, missing)
			return
		}
		send(http.StatusOK, rule)
		return
	}
	if m := statusPath.FindStringSubmatch(p); m != nil {
		all := f.statuses[m[1]]
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit < 1 {
			limit = 30
		}
		lo, hi := min((page-1)*limit, len(all)), min(page*limit, len(all))
		slice := all[lo:hi]
		state := "success"
		if len(all) == 0 {
			state = ""
		}
		var statuses any = slice
		if len(slice) == 0 {
			statuses = nil
		}
		send(http.StatusOK, map[string]any{"state": state, "total_count": len(slice), "statuses": statuses})
		return
	}
	send(http.StatusNotFound, missing)
}

func green(t *testing.T, server *httptest.Server, key, sha string) (bool, string) {
	t.Helper()
	script, err := filepath.Abs("green")
	if err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://git.ca.plug.to/" + owner + "/" + repo + ".git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = checkout
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	cmd := exec.Command("bash", script, sha)
	cmd.Dir = checkout
	cmd.Env = append(cmd.Environ(), "FORGE_URL="+server.URL, "FORGE_TOKEN="+key)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("running green: %v", err)
		}
		return false, string(out)
	}
	return true, string(out)
}

func TestGreen(t *testing.T) {
	cases := []struct {
		name    string
		sha     string
		key     string
		arrange func(*forge)
		pass    bool
		names   string
	}{
		{
			name:  "a commit merged through a pull request whose head carries every required check green passes",
			sha:   merged,
			pass:  true,
			names: "#28",
		},
		{
			name:    "a failed required check refuses",
			sha:     merged,
			arrange: func(f *forge) { f.statuses[head] = with(passing(), "test", "failure") },
			names:   "verify / test (pull_request)",
		},
		{
			name:    "a required check still running refuses",
			sha:     merged,
			arrange: func(f *forge) { f.statuses[head] = with(passing(), "build", "pending") },
			names:   "verify / build (pull_request)",
		},
		{
			name:    "a required check never reported refuses",
			sha:     merged,
			arrange: func(f *forge) { f.statuses[head] = passing()[:3] },
			names:   "verify / security (pull_request)",
		},
		{
			name:    "a commit pushed to the branch without a pull request refuses",
			sha:     pushed,
			arrange: func(f *forge) { f.statuses[pushed] = passing() },
		},
		{
			name: "a pull request that did not merge the commit refuses",
			sha:  head,
			arrange: func(f *forge) {
				f.pulls[head] = pull{Number: 28, Merged: true, MergeCommitSHA: merged, Base: "master", Head: head}
			},
		},
		{
			name: "a merge into a branch other than the default branch refuses",
			sha:  merged,
			arrange: func(f *forge) {
				pr := f.pulls[merged]
				pr.Base = "release-train"
				f.pulls[merged] = pr
				f.protections["release-train"] = f.protections["master"]
			},
			names: "release-train",
		},
		{
			name:    "a default branch that requires no checks vouches for nothing",
			sha:     merged,
			arrange: func(f *forge) { f.protections["master"] = protection{} },
		},
		{
			name:    "an unprotected default branch vouches for nothing",
			sha:     merged,
			arrange: func(f *forge) { delete(f.protections, "master") },
		},
		{
			name: "a failed check protection does not require is ignored",
			sha:  merged,
			arrange: func(f *forge) {
				f.statuses[head] = append(passing(), status{Context: "verify / test (push)", Status: "failure"})
			},
			pass: true,
		},
		{
			name: "a required check on a later page of statuses counts",
			sha:  merged,
			arrange: func(f *forge) {
				var extra []status
				for i := range 120 {
					extra = append(extra, status{Context: "lint / shard " + strconv.Itoa(i), Status: "success"})
				}
				f.statuses[head] = append(extra, with(passing(), "lint (pull_request)", "error")...)
			},
			names: "verify / lint (pull_request): error",
		},
		{
			name: "required contexts match as glob patterns, the way the forge reads them",
			sha:  merged,
			arrange: func(f *forge) {
				f.protections["master"] = protection{EnableStatusCheck: true, StatusCheckContexts: []string{"verify / *"}}
				f.statuses[head] = with(passing(), "security", "failure")
			},
			names: "verify / security (pull_request): failure",
		},
		{
			name: "a glob pattern every check satisfies passes",
			sha:  merged,
			arrange: func(f *forge) {
				f.protections["master"] = protection{EnableStatusCheck: true, StatusCheckContexts: []string{"verify / {lint,test,build,security} (pull_request)"}}
			},
			pass: true,
		},
		{
			name: "a glob pattern no reported check matches refuses",
			sha:  merged,
			arrange: func(f *forge) {
				f.protections["master"] = protection{EnableStatusCheck: true, StatusCheckContexts: append(append([]string(nil), required...), "audit / *")}
			},
			names: "audit / *",
		},
		{
			name: "a token the forge rejects decides nothing",
			sha:  merged,
			key:  "wrong",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newForge()
			if tc.arrange != nil {
				tc.arrange(f)
			}
			server := httptest.NewServer(f)
			defer server.Close()
			key := tc.key
			if key == "" {
				key = token
			}
			pass, out := green(t, server, key, tc.sha)
			if pass != tc.pass {
				t.Fatalf("green passed=%v, want %v\n%s", pass, tc.pass, out)
			}
			if tc.names != "" && !strings.Contains(out, tc.names) {
				t.Fatalf("green output does not name %q\n%s", tc.names, out)
			}
		})
	}
}
