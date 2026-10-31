package platform

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"xupay/internal/cslive"
)

type actionInput struct {
	Proxy          *browserProxyInput `json:"proxy"`
	ExpiresAt      int64              `json:"expires_at"`
	PublishableKey string             `json:"publishable_key"`
	ClientSecret   string             `json:"client_secret"`
	Automatic      bool               `json:"automatic"`
}
type actionCommand struct {
	RequestID int     `json:"request_id,omitempty"`
	Allow     bool    `json:"allow,omitempty"`
	Type      string  `json:"type"`
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	Text      string  `json:"text,omitempty"`
	Key       string  `json:"key,omitempty"`
}
type actionEvent struct {
	RequestID int    `json:"request_id"`
	Type      string `json:"type"`
	Image     string `json:"image"`
	Outcome   string `json:"outcome"`
	Code      string `json:"code"`
	Stage     string `json:"stage"`
	Resource  string `json:"resource"`
	Status    int    `json:"status"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Waiting   bool   `json:"waiting"`
	Details   M      `json:"details"`
}
type actionProcess interface {
	Events() <-chan actionEvent
	Send(actionCommand) error
	Close()
}
type actionStarter func(context.Context, actionInput) (actionProcess, error)
type paymentAction struct {
	mu                     sync.Mutex
	id, actor, state, code string
	frame                  []byte
	expires                time.Time
	process                actionProcess
	cancel                 context.CancelFunc
	automatic              bool
	ctx                    context.Context
	opened                 chan struct{}
}
type nodeAction struct {
	events chan actionEvent
	stdin  io.WriteCloser
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (p *nodeAction) Events() <-chan actionEvent { return p.events }
func (p *nodeAction) Send(v actionCommand) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return json.NewEncoder(p.stdin).Encode(v)
}
func (p *nodeAction) Close() {
	p.once.Do(func() {
		p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			p.cancel()
		}
	})
}
func startActionNode(parent context.Context, in actionInput) (actionProcess, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "node", filepath.Join(filepath.Dir(paymentRuntimePath()), "payment-action.mjs"))
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, errors.New("action runtime unavailable")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		cancel()
		return nil, errors.New("action runtime unavailable")
	}
	if err = cmd.Start(); err != nil {
		stdin.Close()
		cancel()
		return nil, errors.New("action runtime unavailable")
	}
	p := &nodeAction{events: make(chan actionEvent, 4), stdin: stdin, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer close(p.events)
		defer cancel()
		scan := bufio.NewScanner(stdout)
		scan.Buffer(make([]byte, 4096), 1500000)
		for scan.Scan() {
			var e actionEvent
			if json.Unmarshal(scan.Bytes(), &e) != nil {
				continue
			}
			select {
			case p.events <- e:
			case <-ctx.Done():
			}
		}
		cmd.Wait()
	}()
	go func() {
		select {
		case <-parent.Done():
			p.Close()
		case <-p.done:
		}
	}()
	if err = json.NewEncoder(stdin).Encode(in); err != nil {
		p.Close()
		return nil, errors.New("action runtime unavailable")
	}
	return p, nil
}

func (a *App) actionRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/admin/orders/{id}/payment/action", a.require(true, a.checkProtocolPayment))
	m.HandleFunc("GET /api/admin/orders/{id}/payment/action", a.require(true, a.actionView))
	m.HandleFunc("POST /api/admin/orders/{id}/payment/action/input", a.require(true, a.actionInput))
}
func (a *App) actionFor(r *http.Request) *paymentAction {
	a.paymentMu.Lock()
	defer a.paymentMu.Unlock()
	v := a.paymentActions[r.PathValue("id")]
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.actor != current(r).ID {
		return nil
	}
	return v
}

// Claim the already prepared entry without starting a second worker or changing
// the saved payment. Only its first administrator can view/control the browser.
func (a *App) openPreparedPaymentAction(w http.ResponseWriter, r *http.Request) bool {
	a.paymentMu.Lock()
	defer a.paymentMu.Unlock()
	v := a.paymentActions[r.PathValue("id")]
	if v == nil || v.opened == nil || v.ctx == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.expires.After(time.Now()) || v.state == "finished" || v.state == "closed" || v.ctx.Err() != nil {
		return false
	}
	if v.actor != "" && v.actor != current(r).ID {
		fail(w, 409, "另一位管理员正在处理原付款验证")
		return true
	}
	if v.actor == "" {
		v.actor, v.state = current(r).ID, "preparing"
		close(v.opened)
	}
	out(w, 202, M{"accepted": true, "action_id": v.id})
	return true
}
func (a *App) actionView(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a.paymentMu.Lock()
	entry := a.paymentActions[r.PathValue("id")]
	if entry != nil {
		entry.mu.Lock()
		available := entry.actor == "" && entry.state == "available" && entry.expires.After(time.Now()) && entry.ctx != nil && entry.ctx.Err() == nil
		expiry := entry.expires
		entry.mu.Unlock()
		if available {
			a.paymentMu.Unlock()
			out(w, 200, M{"state": "available", "expires_at": expiry})
			return
		}
	}
	a.paymentMu.Unlock()
	v := a.actionFor(r)
	if v == nil {
		out(w, 200, M{"state": "idle"})
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	image := ""
	if len(v.frame) > 0 && v.expires.After(time.Now()) {
		image = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(v.frame)
	}
	out(w, 200, M{"id": v.id, "state": v.state, "code": v.code, "image": image, "expires_at": v.expires, "width": 960, "height": 720})
}
func validActionCommand(v actionCommand) bool {
	switch v.Type {
	case "close":
		return true
	case "click":
		return v.X >= 0 && v.X < 960 && v.Y >= 0 && v.Y < 720
	case "scroll":
		return v.Y >= -600 && v.Y <= 600
	case "text":
		return len(v.Text) > 0 && utf8.ValidString(v.Text) && utf8.RuneCountInString(v.Text) <= 128 && strings.IndexFunc(v.Text, func(r rune) bool { return r < 32 || r == 127 }) < 0
	case "key":
		return strings.Contains("|Tab|Enter|Escape|Backspace|Delete|ArrowUp|ArrowDown|ArrowLeft|ArrowRight|Space|", "|"+v.Key+"|") && v.Key != ""
	}
	return false
}
func (a *App) actionInput(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID string `json:"id"`
		actionCommand
	}
	if !read(w, r, &input) {
		return
	}
	if !validActionCommand(input.actionCommand) {
		fail(w, 400, "验证操作无效")
		return
	}
	v := a.actionFor(r)
	if v == nil {
		fail(w, 404, "验证窗口不存在")
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.id != input.ID || !v.expires.After(time.Now()) || v.state == "finished" || v.state == "closed" {
		fail(w, 409, "验证窗口已结束，请核实原交易")
		return
	}
	if input.Type == "close" {
		v.cancel()
		v.frame = nil
		v.state = "closed"
		out(w, 200, M{"ok": true})
		return
	}
	if v.process == nil {
		fail(w, 409, "验证窗口正在准备")
		return
	}
	if err := v.process.Send(input.actionCommand); err != nil {
		fail(w, 409, "验证窗口已关闭")
		return
	}
	out(w, 200, M{"ok": true})
}

var actionPublishableKey = regexp.MustCompile(`^pk_[A-Za-z0-9_]+$`)
var actionClientSecret = regexp.MustCompile(`^pi_[A-Za-z0-9]+_secret_[A-Za-z0-9]+$`)

func validPaymentActionCheckpoint(cp cslive.Checkpoint) bool {
	return cp.Payment == "action_required" && paymentIntentID.MatchString(cp.PaymentIntentID) && strings.HasPrefix(cp.ClientSecret, cp.PaymentIntentID+"_secret_") && actionClientSecret.MatchString(cp.ClientSecret) && actionPublishableKey.MatchString(cp.PublishableKey)
}

func (r *protocolRun) runPaymentAction(cp cslive.Checkpoint, v *paymentAction) (resultErr error) {
	started := time.Now()
	code := ""
	if v.automatic {
		code = "automatic_verification"
	}
	r.a.paymentTrace(r.ctx, r.id, "verification", "running", code, 0)
	defer func() {
		if resultErr != nil {
			r.a.paymentTrace(r.ctx, r.id, "verification", "review", protocolPublicCode(cslive.ErrorCode(resultErr)), time.Since(started))
		}
		v.mu.Lock()
		v.process, v.frame = nil, nil
		v.mu.Unlock()
	}()
	if !validPaymentActionCheckpoint(cp) {
		return cslive.Failure("payment_intent_state_unavailable")
	}
	var enc string
	var expires time.Time
	if err := r.a.DB.QueryRow(r.ctx, "SELECT endpoint_enc,expires_at FROM order_proxy_sessions WHERE order_id=$1", r.id).Scan(&enc, &expires); err != nil {
		return cslive.Failure("proxy_session_unavailable")
	}
	endpoint, err := r.a.decrypt(enc)
	if err != nil {
		return cslive.Failure("proxy_session_unavailable")
	}
	if err = r.guardProxy(r.ctx); err != nil {
		return err
	}
	if _, err = r.a.orderExitIP(r.ctx, r.id, r.guardProxy, true); err != nil {
		return err
	}
	deadline := minTime(expires, time.Now().Add(10*time.Minute))
	if !v.expires.IsZero() {
		deadline = minTime(deadline, v.expires)
	}
	ctx, cancel := context.WithDeadline(r.ctx, deadline)
	defer cancel()
	normalized, err := normalizeProxy(endpoint)
	if err != nil {
		return cslive.Failure("proxy_session_unavailable")
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return cslive.Failure("proxy_session_unavailable")
	}
	dial, err := configuredProxyDialer()
	if err != nil {
		return cslive.Failure("proxy_session_unavailable")
	}
	// Only this private, server-controlled browser may visit public bank/SDK
	// verification hosts; no inbound client supplies a URL to this relay.
	bridge, closeBridge, err := startBrowserProxyChecked(ctx, func(ctx context.Context, target string) (net.Conn, error) {
		return upstreamProxyTunnel(ctx, u, target, dial)
	}, func(ctx context.Context, host string) bool {
		if net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return false
		}
		if _, e := publicDialAddress(ctx, net.JoinHostPort(host, "443")); e != nil {
			return false
		}
		return true // Every HTTP attempt also checks the lease over private IPC.
	})
	if err != nil {
		return cslive.Failure("proxy_session_unavailable")
	}
	defer closeBridge()
	expiry, _ := ctx.Deadline()
	v.mu.Lock()
	v.expires = expiry
	v.mu.Unlock()
	starter := r.a.actionStart
	if starter == nil {
		starter = startActionNode
	}
	process, err := starter(ctx, actionInput{Proxy: bridge, ExpiresAt: expiry.Unix(), PublishableKey: cp.PublishableKey, ClientSecret: cp.ClientSecret, Automatic: v.automatic})
	if err != nil {
		return cslive.Failure("action_runtime_unavailable")
	}
	defer process.Close()
	v.mu.Lock()
	v.process = process
	v.mu.Unlock()
	completed := false
	runtimeCode := "action_interrupted"
	// Browser resources arrive concurrently. Share only an in-flight check;
	// never reuse a completed lease check or block the IPC reader behind database IO.
	guardDone := make(chan error, 1)
	pendingGuards := []int{}
	var terminal *actionEvent
	eventStream := process.Events()
events:
	for {
		var e actionEvent
		if terminal != nil && len(pendingGuards) == 0 {
			e, terminal = *terminal, nil
		} else {
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					runtimeCode = "action_timeout"
				}
				break events
			case guardErr := <-guardDone:
				if guardErr != nil {
					runtimeCode = cslive.ErrorCode(guardErr)
					break events
				}
				for _, id := range pendingGuards {
					if process.Send(actionCommand{Type: "guard", RequestID: id, Allow: true}) != nil {
						break events
					}
				}
				pendingGuards = nil
				continue
			case event, ok := <-eventStream:
				if !ok {
					if len(pendingGuards) == 0 {
						break events
					}
					eventStream = nil
					event = actionEvent{Type: "interrupted"}
				}
				e = event
			}
		}
		if e.Type == "guard" {
			if len(pendingGuards) >= 256 {
				runtimeCode = "action_guard_overloaded"
				break events
			}
			pendingGuards = append(pendingGuards, e.RequestID)
			if len(pendingGuards) == 1 {
				go func() {
					check, stop := context.WithTimeout(ctx, 10*time.Second)
					defer stop()
					guardDone <- r.guardProxy(check)
				}()
			}
			continue
		}
		if len(pendingGuards) > 0 && (e.Type == "result" || e.Type == "error" || e.Type == "timeout" || e.Type == "interrupted") {
			terminal, eventStream = &e, nil
			continue
		}
		if e.Type == "progress" || e.Type == "diagnostic" || e.Type == "network" {
			details := safeDiagnosticDetails(e.Details)
			for key, value := range (M{"sdk_stage": e.Stage, "sdk_resource": e.Resource, "sdk_event": e.Type, "sdk_issue": e.Code, "sdk_wait_ms": e.ElapsedMS, "sdk_waiting": e.Waiting}) {
				details[key] = value
			}
			// Resource completion bursts must not block the IPC reader or delay
			// original-IP guard replies behind diagnostic database writes.
			r.a.enqueueDiagnostic(providerDiagnostic{Order: r.id, Phase: "verification", At: time.Now().UTC(), Status: e.Status, Details: details})
			continue
		}
		v.mu.Lock()
		switch e.Type {
		case "ready":
			v.state = "verifying"
		case "frame":
			if b, err := base64.StdEncoding.DecodeString(e.Image); !v.automatic && err == nil && len(b) <= 1000000 && len(b) > 3 && b[0] == 0xff && b[1] == 0xd8 {
				v.frame = b
			}
		case "timeout":
			runtimeCode = "action_timeout"
		case "error":
			v.code = "action_runtime_unavailable"
			runtimeCode = v.code
		case "result":
			completed = true
			v.code = safeActionCode(e.Code)
			v.state = "checking"
			v.frame = nil
			r.a.saveProviderDiagnostic(providerDiagnostic{Order: r.id, Phase: "verification", At: time.Now().UTC(), Elapsed: time.Since(started), Details: M{"sdk_outcome": safeActionOutcome(e.Outcome), "sdk_code": v.code}})
		}
		v.mu.Unlock()
		if completed || e.Type == "error" || e.Type == "timeout" || e.Type == "interrupted" {
			break events
		}
	}
	if !completed {
		return cslive.Failure(runtimeCode)
	}
	// The browser result is advisory. Recover reads the original transaction;
	// only authoritative provider evidence settles money and continues renewal.
	return nil
}
func safeActionCode(s string) string {
	switch s {
	case "", "authentication_required", "payment_intent_authentication_failure", "card_declined", "expired_card", "incorrect_cvc", "processing_error", "preflight_failed", "context_mismatch", "action_not_required", "csp_blocked", "sdk_unavailable", "payment_intent_unexpected_state", "payment_intent_invalid_parameter", "invalid_client_secret", "resource_missing", "api_connection_error", "sdk_error", "unknown":
		return s
	}
	return "unknown"
}

func safeActionOutcome(s string) string {
	switch s {
	case "sdk_returned", "sdk_error", "sdk_exception", "page_error":
		return s
	}
	return "unknown"
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
