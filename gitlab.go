package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type gitlab struct{ base, token string }

func newGitlab(c *Config, token string) gitlab {
	return gitlab{base: strings.TrimRight(c.GitLab.URL, "/") + "/api/v4/projects/" + url.PathEscape(c.GitLab.Project), token: token}
}

type pipeline struct {
	ID     int64  `json:"id"`
	SHA    string `json:"sha"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
}

func (g gitlab) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, g.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token) // accepted for personal, project and OAuth tokens
	req.Header.Set("Content-Type", "application/json")
	return doJSON(req, out)
}

func (g gitlab) branchHead(branch string) (string, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	err := g.do(http.MethodGet, "/repository/branches/"+url.PathEscape(branch), nil, &b)
	return b.Commit.ID, err
}

func (g gitlab) commitsBetween(from, to string) (int, error) {
	var c struct {
		Commits []json.RawMessage `json:"commits"`
	}
	err := g.do(http.MethodGet, "/repository/compare?straight=true&from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to), nil, &c)
	return len(c.Commits), err
}

// triggerPipeline creates a pipeline on ref, passing vals as spec:inputs
// (as = "inputs") or as CI variables.
func (g gitlab) triggerPipeline(ref string, vals map[string]string, as string) (pipeline, error) {
	body := map[string]any{"ref": ref}
	if as == "variables" {
		type kv struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		vars := []kv{}
		for k, v := range vals {
			vars = append(vars, kv{k, v})
		}
		body["variables"] = vars
	} else {
		body["inputs"] = vals
	}
	var p pipeline
	err := g.do(http.MethodPost, "/pipeline", body, &p)
	return p, err
}

type deployment struct {
	SHA        string    `json:"sha"`
	UpdatedAt  time.Time `json:"updated_at"`
	Deployable struct {
		Pipeline struct {
			WebURL string `json:"web_url"`
		} `json:"pipeline"`
	} `json:"deployable"`
}

// lastDeployment is the environment's latest successful deployment, nil if none.
func (g gitlab) lastDeployment(env string) (*deployment, error) {
	q := url.Values{"environment": {env}, "status": {"success"}, "order_by": {"id"}, "sort": {"desc"}, "per_page": {"1"}}
	var ds []deployment
	if err := g.do(http.MethodGet, "/deployments?"+q.Encode(), nil, &ds); err != nil || len(ds) == 0 {
		return nil, err
	}
	return &ds[0], nil
}

func (g gitlab) pipeline(id int64) (pipeline, error) {
	var p pipeline
	err := g.do(http.MethodGet, fmt.Sprintf("/pipelines/%d", id), nil, &p)
	return p, err
}

// memberLevel returns the user's effective access level on the project
// (inherited from groups included), 0 when not a member.
func (g gitlab) memberLevel(userID int64) (int, error) {
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	err := g.do(http.MethodGet, fmt.Sprintf("/members/all/%d", userID), nil, &m)
	if he, ok := errors.AsType[*httpError](err); ok && he.code == http.StatusNotFound {
		return 0, nil
	}
	return m.AccessLevel, err
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func doJSON(req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &httpError{resp.StatusCode, fmt.Sprintf("%s %s: %s %s", req.Method, req.URL.Path, resp.Status, bytes.TrimSpace(msg))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
