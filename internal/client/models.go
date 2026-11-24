package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// The vendors' model lists the runner adds to a CLI's own for an API key
// (decision 2026-11-24-model-lists); variables so tests can serve them.
var (
	anthropicModels = "https://api.anthropic.com/v1/models?limit=1000"
	googleModels    = "https://generativelanguage.googleapis.com/v1beta/models?pageSize=1000"
)

// maxListBody is the most of a model list read; a longer one is not read.
const maxListBody = 4 << 20

// askScript is node's part in a listing: it starts the CLI (its argv), writes
// the request lines of $REQ to it and prints the CLI's first JSON line that
// answers the request: a control_response to "models" or the JSON-RPC answer
// with id 1. The CLI is stopped then; it runs no turn. The child gets no
// LD_LIBRARY_PATH: on musl, the tools' libraries are node's only.
const askScript = `const env={...process.env};delete env.LD_LIBRARY_PATH;delete env.REQ;
const c=require('child_process').spawn(process.argv[1],process.argv.slice(2),{env,stdio:['pipe','pipe','inherit']});
setTimeout(()=>{c.kill();process.exit(2)},60000);
let buf='';
c.stdout.on('data',d=>{buf+=d;let i;while((i=buf.indexOf('\n'))>=0){const l=buf.slice(0,i);buf=buf.slice(i+1);let o;try{o=JSON.parse(l)}catch{continue}
if(o.id===1||o.response&&o.response.request_id==='models'){console.log(l);c.kill();process.exit(0)}}});
c.on('close',()=>process.exit(3));
c.stdin.write(process.env.REQ);`

// The requests of a listing: Claude Code's initialize control request, as
// the Agent SDK's supportedModels sends it, and Codex app-server's
// initialize and model/list.
const (
	claudeAsk = `{"type":"control_request","request_id":"models","request":{"subtype":"initialize"}}` + "\n"
	codexAsk  = `{"id":0,"method":"initialize","params":{"clientInfo":{"name":"runner","version":"1"}}}` + "\n" +
		`{"method":"initialized"}` + "\n" +
		`{"id":1,"method":"model/list","params":{"limit":500}}` + "\n"
)

// Models are the models the CLI can run with its credential, its default
// first (decision 2026-11-24-model-lists): what the CLI itself lists for its
// account, asked in a short container of image with its state volume and
// credential, without a turn. For a Claude Code API key it is Anthropic's
// list after the CLI's default (Willem, CLR-218, answer B); for a Gemini API
// key, Google's generateContent models follow the CLI's list (decision
// all-gemini-models in the app repository). OpenCode has none: nil, nil, as
// its models are its providers'. c.Tools must be set.
func (c *CLI) Models(ctx context.Context, image string) ([]string, error) {
	var ids []string
	var err error
	switch c.Name {
	case "claude":
		var out string
		if out, err = c.list(ctx, image, append([]string{tools.Path + "/bin/node", "-e", askScript, tools.Path + "/bin/claude"}, claudeListArgs...), claudeAsk); err == nil {
			ids, err = parseClaudeModels(out)
		}
		if err == nil && c.Credential != "" && !strings.HasPrefix(c.Credential, "sk-ant-oat") {
			var api []string
			api, err = fetchIDs(ctx, anthropicModels, map[string]string{"x-api-key": c.Credential, "anthropic-version": "2023-06-01"}, anthropicIDs)
			ids = defaultFirst(ids[:min(1, len(ids))], api)
		}
	case "codex":
		argv := []string{tools.Path + "/bin/node", "-e", askScript, "sh", "-c", codexShell, tools.Path + "/bin/codex"}
		if c.BaseURL != "" {
			argv = append(argv, "-c", "openai_base_url="+strconv.Quote(c.BaseURL))
		}
		var out string
		if out, err = c.list(ctx, image, append(argv, "app-server"), codexAsk); err == nil {
			ids, err = parseCodexModels(out)
		}
	case "gemini":
		var out string
		argv := []string{"sh", "-c", geminiShell(`"$` + geminiSettingsVar + `"`), tools.Path + "/bin/node", tools.Path + "/gemini/bundle/gemini.js", "models", "list", "--output-format", "json"}
		if out, err = c.list(ctx, image, argv, ""); err == nil {
			ids, err = parseGeminiModels(out)
		}
		if err == nil && c.Credential != "" && !c.Vertex {
			var api []string
			api, err = fetchIDs(ctx, googleModels, map[string]string{"x-goog-api-key": c.Credential}, googleIDs)
			ids = defaultFirst(ids, api)
		}
	default:
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s models: %w", c.Name, err)
	}
	return ids, nil
}

// claudeListArgs start Claude Code to answer control requests on stdin.
var claudeListArgs = []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}

// list runs argv in a container of image with the client's state volume, the
// tools and the client's environment, as a turn has them but on Docker's
// default network, as detection's key check; the request goes in as $REQ.
// Every value of the environment goes by name: it may be the credential.
func (c *CLI) list(ctx context.Context, image string, argv []string, req string) (string, error) {
	k := kinds[c.Name]
	if err := PrepareState(ctx, c.Exec, c.Project, c.Name); err != nil {
		return "", err
	}
	env := slices.Concat(c.Tools.Env, c.Tools.OpenCodeEnv, k.env(c, Turn{}))
	if req != "" {
		env = append(env, "REQ="+req)
	}
	docker := []string{"docker", "run", "--rm", "--user", workspace.AgentUser, "-v", StateVolume(c.Project, c.Name) + ":" + StatePath}
	docker = append(docker, c.Tools.Args...)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		docker = append(docker, "-e", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // the script stops the CLI after one
	defer cancel()
	return c.Exec(ctx, env, append(append(docker, image), argv...)...)
}

// parseClaudeModels reads Claude Code's answer to initialize: its models,
// each as the full id an alias resolves to, the current model first.
func parseClaudeModels(out string) ([]string, error) {
	for _, line := range jsonLines(out) {
		var r struct {
			Type     string `json:"type"`
			Response struct {
				RequestID string `json:"request_id"`
				Error     string `json:"error"`
				Response  *struct {
					Models []struct {
						Value         string `json:"value"`
						ResolvedModel string `json:"resolvedModel"`
					} `json:"models"`
					CurrentModel string `json:"current_model"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(line), &r) != nil || r.Type != "control_response" || r.Response.RequestID != "models" {
			continue
		}
		if r.Response.Response == nil {
			return nil, fmt.Errorf("initialize refused: %s", r.Response.Error)
		}
		ids := []string{}
		current := r.Response.Response.CurrentModel
		for _, m := range r.Response.Response.Models {
			id := m.ResolvedModel
			if id == "" && m.Value != "default" {
				id = m.Value
			}
			if m.Value == current && id != "" {
				current = id
			}
			if id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		if current == "" || current == "default" {
			return ids, nil
		}
		return defaultFirst([]string{current}, ids), nil
	}
	return nil, errors.New("no answer to initialize")
}

// parseCodexModels reads Codex app-server's answer to model/list: the models
// it offers, hidden ones left out, its default first.
func parseCodexModels(out string) ([]string, error) {
	for _, line := range jsonLines(out) {
		var r struct {
			ID     *int `json:"id"`
			Result *struct {
				Data []struct {
					ID        string `json:"id"`
					Model     string `json:"model"`
					Hidden    bool   `json:"hidden"`
					IsDefault bool   `json:"isDefault"`
				} `json:"data"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &r) != nil || r.ID == nil || *r.ID != 1 {
			continue
		}
		if r.Result == nil {
			if r.Error != nil {
				return nil, fmt.Errorf("model/list refused: %s", r.Error.Message)
			}
			return nil, errors.New("model/list answered no result")
		}
		var def, ids []string
		for _, m := range r.Result.Data {
			id := m.Model
			if id == "" {
				id = m.ID
			}
			switch {
			case id == "" || m.Hidden || slices.Contains(ids, id):
			case m.IsDefault:
				def = []string{id}
			default:
				ids = append(ids, id)
			}
		}
		return defaultFirst(def, ids), nil
	}
	return nil, errors.New("no answer to model/list")
}

// parseGeminiModels reads `gemini models list --output-format json`: a JSON
// array, printed over several lines, whose first entry is the CLI's default.
func parseGeminiModels(out string) ([]string, error) {
	for i := 0; i < len(out); i++ {
		if out[i] != '[' || i > 0 && out[i-1] != '\n' {
			continue
		}
		var list []struct {
			ModelID string `json:"modelId"`
		}
		if json.NewDecoder(strings.NewReader(out[i:])).Decode(&list) != nil {
			continue
		}
		ids := []string{}
		for _, m := range list {
			if m.ModelID != "" && !slices.Contains(ids, m.ModelID) {
				ids = append(ids, m.ModelID)
			}
		}
		return ids, nil
	}
	return nil, errors.New("no model list")
}

// defaultFirst is first, then the ids of rest not in it; never nil.
func defaultFirst(first, rest []string) []string {
	out := append([]string{}, first...)
	for _, id := range rest {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// anthropicIDs are the ids of Anthropic's model list.
func anthropicIDs(body io.Reader) ([]string, error) {
	var list struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&list); err != nil || list.Data == nil {
		return nil, fmt.Errorf("not a model list: %v", err)
	}
	ids := []string{}
	for _, m := range *list.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// googleIDs are the Gemini API's models that generate content, without
// their "models/" prefix.
func googleIDs(body io.Reader) ([]string, error) {
	var list struct {
		Models *[]struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.NewDecoder(body).Decode(&list); err != nil || list.Models == nil {
		return nil, fmt.Errorf("not a model list: %v", err)
	}
	ids := []string{}
	for _, m := range *list.Models {
		if slices.Contains(m.Methods, "generateContent") {
			ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
		}
	}
	return ids, nil
}

// fetchIDs is the ids of the model list at url, asked with headers, read by
// parse. shortcut: one page; both vendors list fewer models than a page holds.
func fetchIDs(ctx context.Context, url string, headers map[string]string, parse func(io.Reader) ([]string, error)) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("answered %s", resp.Status)
	}
	return parse(io.LimitReader(resp.Body, maxListBody))
}
