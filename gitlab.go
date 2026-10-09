package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
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

func (g gitlab) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token) // accepted for personal, project and OAuth tokens
	req.Header.Set("Content-Type", "application/json")
	return doJSON(req, out)
}

func (g gitlab) branchHead(ctx context.Context, branch string) (string, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	err := g.do(ctx, http.MethodGet, "/repository/branches/"+url.PathEscape(branch), nil, &b)
	return b.Commit.ID, err
}

func (g gitlab) commitsBetween(ctx context.Context, from, to string) (int, error) {
	var c struct {
		Commits []json.RawMessage `json:"commits"`
	}
	err := g.do(ctx, http.MethodGet, "/repository/compare?straight=true&from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to), nil, &c)
	return len(c.Commits), err
}

// triggerPipeline creates a pipeline on ref, passing vals as spec:inputs
// (as = "inputs") or as CI variables.
func (g gitlab) triggerPipeline(ctx context.Context, ref string, vals map[string]string, as string) (pipeline, error) {
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
	err := g.do(ctx, http.MethodPost, "/pipeline", body, &p)
	return p, err
}

type deployment struct {
	SHA        string    `json:"sha"`
	Status     string    `json:"status"` // created, running, success, failed, canceled, blocked
	UpdatedAt  time.Time `json:"updated_at"`
	Deployable struct {
		Pipeline struct {
			WebURL string `json:"web_url"`
		} `json:"pipeline"`
	} `json:"deployable"`
}

// lastDeployment is the environment's latest deployment with the given
// status ("" = any), nil if none.
func (g gitlab) lastDeployment(ctx context.Context, env, status string) (*deployment, error) {
	q := url.Values{"environment": {env}, "order_by": {"id"}, "sort": {"desc"}, "per_page": {"1"}}
	if status != "" {
		q.Set("status", status)
	}
	var ds []deployment
	if err := g.do(ctx, http.MethodGet, "/deployments?"+q.Encode(), nil, &ds); err != nil || len(ds) == 0 {
		return nil, err
	}
	return &ds[0], nil
}

// running is what an environment runs: its latest successful deployment, and
// the latest one too when that failed or was canceled (the one before stays up).
func (g gitlab) running(ctx context.Context, env string) (ok, failed *deployment, err error) {
	last, err := g.lastDeployment(ctx, env, "")
	if err != nil || last == nil || last.Status == "success" {
		return last, nil, err
	}
	ok, err = g.lastDeployment(ctx, env, "success")
	if last.Status == "failed" || last.Status == "canceled" {
		failed = last
	}
	return ok, failed, err
}

// stoppedEnvironments names the project's stopped environments: their pods
// were torn down.
func (g gitlab) stoppedEnvironments(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	for page := 1; ; page++ {
		var envs []struct {
			Name string `json:"name"`
		}
		if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/environments?states=stopped&per_page=100&page=%d", page), nil, &envs); err != nil {
			return nil, err
		}
		for _, e := range envs {
			out[e.Name] = true
		}
		if len(envs) < 100 {
			return out, nil
		}
	}
}

// Pods counts a cell's pods, read through the GitLab agent for Kubernetes.
type Pods struct {
	Ready int    `json:"ready"`
	Total int    `json:"total"`
	Error string `json:"error,omitempty"`
}

type podCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// countPods lists the pods matching selector in namespace through the agent's
// k8s-proxy. GitLab authenticates the call with "pat:<agent id>:<token>" and
// forwards it to that agent's cluster. Completed pods don't count.
func countPods(ctx context.Context, proxy string, agentID int64, token, namespace, selector string) (Pods, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	u := fmt.Sprintf("%s/api/v1/namespaces/%s/pods?labelSelector=%s", proxy, url.PathEscape(namespace), url.QueryEscape(selector))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Pods{}, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer pat:%d:%s", agentID, token))
	var list struct {
		Items []struct {
			Metadata struct {
				DeletionTimestamp *time.Time `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase      string         `json:"phase"`
				Conditions []podCondition `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := doJSON(req, &list); err != nil {
		return Pods{}, err
	}
	var p Pods
	for _, it := range list.Items {
		if it.Status.Phase == "Succeeded" || it.Status.Phase == "Failed" {
			continue
		}
		p.Total++
		ready := it.Status.Phase == "Running" && it.Metadata.DeletionTimestamp == nil &&
			slices.Contains(it.Status.Conditions, podCondition{"Ready", "True"})
		if ready {
			p.Ready++
		}
	}
	return p, nil
}

func (g gitlab) pipeline(ctx context.Context, id int64) (pipeline, error) {
	var p pipeline
	err := g.do(ctx, http.MethodGet, fmt.Sprintf("/pipelines/%d", id), nil, &p)
	return p, err
}

// memberLevel returns the user's effective access level on the project
// (inherited from groups included), 0 when not a member.
func (g gitlab) memberLevel(ctx context.Context, userID int64) (int, error) {
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	err := g.do(ctx, http.MethodGet, fmt.Sprintf("/members/all/%d", userID), nil, &m)
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
