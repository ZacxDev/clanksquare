package editor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// ErrSpendCap is returned, before any request is sent, by a call whose
// worst-case cost would take the meter over its cap.
var ErrSpendCap = errors.New("spend cap reached")

// Meter enforces a USD spend cap across model calls. Each call reserves its
// worst-case estimate first and is refused if that would cross the cap; once
// it answers, the reservation is replaced by the cost OpenRouter reports
// (usage.cost), or kept when none is reported.
//
// With a Ledger path, the running total is read from and written to that
// file, so one cap holds across runs and commands.
type Meter struct {
	Cap    float64
	Ledger string

	mu    sync.Mutex
	spent float64
	calls int
}

type ledgerFile struct {
	SpentUSD float64 `json:"spent_usd"`
	Calls    int     `json:"calls"`
}

// NewMeter returns a meter with the given cap, loading ledger if it exists.
func NewMeter(capUSD float64, ledger string) (*Meter, error) {
	m := &Meter{Cap: capUSD, Ledger: ledger}
	if ledger == "" {
		return m, nil
	}
	raw, err := os.ReadFile(ledger)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var lf ledgerFile
	if err := json.Unmarshal(raw, &lf); err != nil {
		return nil, fmt.Errorf("spend ledger %s: %w", ledger, err)
	}
	m.spent, m.calls = lf.SpentUSD, lf.Calls
	return m, nil
}

// Spent is the total so far.
func (m *Meter) Spent() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spent
}

// Calls is the number of settled calls so far.
func (m *Meter) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *Meter) reserve(est float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.spent+est > m.Cap {
		return fmt.Errorf("%w: spent $%.6f + this call's worst case $%.6f > cap $%.4f", ErrSpendCap, m.spent, est, m.Cap)
	}
	m.spent += est
	return nil
}

// settle replaces a reservation with the actual cost (actual < 0: unknown,
// keep the estimate) and persists the ledger.
func (m *Meter) settle(est, actual float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if actual >= 0 {
		m.spent += actual - est
	}
	m.calls++
	if m.Ledger == "" {
		return nil
	}
	raw, _ := json.Marshal(ledgerFile{SpentUSD: m.spent, Calls: m.calls})
	return hubkit.WriteFileAtomic(m.Ledger, append(raw, '\n'))
}

// Model is an OpenRouter client. Key is the API key; it is only ever put in
// the Authorization header.
type Model struct {
	Base   string // e.g. https://openrouter.ai
	Key    string
	HTTP   *http.Client
	Meter  *Meter
	Prices map[string][2]float64
}

const (
	modelTimeout = 90 * time.Second
	maxModelResp = 32 << 20
)

// overheadTokens is added to every estimate: a decision model wraps the
// request in its own prompt (Classify-1 billed 567 input tokens for a
// 900-byte request on 2026-11-08), so bytes alone underestimate.
const overheadTokens = 2000

// estimate is the worst-case USD cost of a call: inputBytes/2 tokens (a
// conservative bytes-per-token) plus overheadTokens, plus maxOut output
// tokens, at the model's prices. ok is false when the model has no price.
func (m *Model) estimate(model string, inputBytes, maxOut int) (float64, bool) {
	p, ok := m.Prices[model]
	if !ok {
		return 0, false
	}
	return ((float64(inputBytes)/2+overheadTokens)*p[0] + float64(maxOut)*p[1]) / 1e6, true
}

// APIError is a non-200 answer.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("openrouter HTTP %d: %s", e.Status, e.Body) }

// post sends one metered JSON request and decodes the answer into out.
// costOf reads the reported USD cost from the decoded answer (< 0: none).
func (m *Model) post(ctx context.Context, path, model string, req any, maxOut int, out any, costOf func() float64) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	est, ok := m.estimate(model, len(body), maxOut)
	if !ok {
		return fmt.Errorf("model %s has no price in the config: refusing to call it (the spend cap needs one)", model)
	}
	if err := m.Meter.reserve(est); err != nil {
		return err
	}
	actual := -1.0
	defer func() { m.Meter.settle(est, actual) }()

	ctx, cancel := context.WithTimeout(ctx, modelTimeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.Base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Authorization", "Bearer "+m.Key)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("X-Title", "clanksquare-editor")
	hr.Header.Set("HTTP-Referer", "https://github.com/ZacxDev/clanksquare")
	resp, err := m.HTTP.Do(hr)
	if err != nil {
		return err // the request may have been billed; the estimate stays
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxModelResp))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		actual = 0 // an error answer is not billed
		return &APIError{Status: resp.StatusCode, Body: clipStr(string(raw), 300)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("openrouter %s: bad JSON: %w", path, err)
	}
	actual = costOf()
	return nil
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

type usage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	InputTokens      int      `json:"input_tokens"`
	Cost             *float64 `json:"cost"`
}

func (u usage) cost() float64 {
	if u.Cost == nil {
		return -1
	}
	return *u.Cost
}

// Embed returns one embedding per input, in input order.
func (m *Model) Embed(ctx context.Context, model string, inputs []string) ([][]float64, error) {
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage usage `json:"usage"`
	}
	req := map[string]any{"model": model, "input": inputs}
	if err := m.post(ctx, "/api/v1/embeddings", model, req, 0, &out, func() float64 { return out.Usage.cost() }); err != nil {
		return nil, err
	}
	res := make([][]float64, len(inputs))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(inputs) || len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embeddings: bad item index %d", d.Index)
		}
		res[d.Index] = d.Embedding
	}
	for i, e := range res {
		if e == nil {
			return nil, fmt.Errorf("embeddings: no vector for input %d", i)
		}
	}
	return res, nil
}

// Question is one Decisions API question.
type Question struct {
	Type         string `json:"type"` // choice | noul
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Answer is one Decisions API answer. Check Type before reading fields.
type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Noul          *float64           `json:"noul"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

// Decision is a Decisions API answer set and the exact model build.
type Decision struct {
	Model   string
	Answers map[string]Answer
}

// Decide calls POST /api/alpha/decisions. ScaleDown's models accept a state
// that is a string or {"text": …}; the editor always sends {"text": <JSON>}
// so the same request works on every decision model.
func (m *Model) Decide(ctx context.Context, model string, state any, questions map[string]Question) (*Decision, error) {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	var out struct {
		Model   string            `json:"model"`
		Answers map[string]Answer `json:"answers"`
		Usage   usage             `json:"usage"`
	}
	req := map[string]any{"model": model, "state": map[string]string{"text": string(stateJSON)}, "questions": questions}
	if err := m.post(ctx, "/api/alpha/decisions", model, req, 0, &out, func() float64 { return out.Usage.cost() }); err != nil {
		return nil, err
	}
	for k, q := range questions {
		a, ok := out.Answers[k]
		if !ok || a.Type != q.Type {
			return nil, fmt.Errorf("decisions: answer %q missing or of type %q, want %q", k, a.Type, q.Type)
		}
		if q.Type == "noul" && a.Noul == nil {
			return nil, fmt.Errorf("decisions: noul answer %q has no value", k)
		}
		if q.Type == "choice" && a.Probabilities == nil {
			return nil, fmt.Errorf("decisions: choice answer %q has no probabilities", k)
		}
	}
	return &Decision{Model: out.Model, Answers: out.Answers}, nil
}

// Chat calls /api/v1/chat/completions and returns the assistant's text.
func (m *Model) Chat(ctx context.Context, model, system, user string, maxTokens int) (string, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	req := map[string]any{
		"model":       model,
		"max_tokens":  maxTokens,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"usage": map[string]bool{"include": true},
	}
	if err := m.post(ctx, "/api/v1/chat/completions", model, req, maxTokens, &out, func() float64 { return out.Usage.cost() }); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("chat: no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// LoadAPIKey reads the OpenRouter key from $OPENROUTER_API_KEY or, failing
// that, from the file named by path. It never takes the key as an argument.
func LoadAPIKey(path string) (string, error) {
	if k := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")); k != "" {
		return k, nil
	}
	if path == "" {
		return "", errors.New("no OpenRouter key: set OPENROUTER_API_KEY or pass -openrouter-key-file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("openrouter key file: %w", err)
	}
	k := strings.TrimSpace(string(raw))
	if k == "" {
		return "", errors.New("openrouter key file is empty")
	}
	return k, nil
}
