package platform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

type cardInput struct {
	Number string `json:"number"`
	Month  string `json:"month"`
	Year   string `json:"year"`
	CVC    string `json:"cvc"`
}
type paymentInput struct {
	Executor        string                                               `json:"-"`
	Authorization   checkoutInput                                        `json:"-"`
	Checkpoint      legacyPaymentCheckpoint                              `json:"-"`
	Guard           func(context.Context) error                          `json:"-"`
	SaveCheckpoint  func(context.Context, legacyPaymentCheckpoint) error `json:"-"`
	ResumeApproval  bool                                                 `json:"resume_approval,omitempty"`
	SavedQuote      M                                                    `json:"saved_quote,omitempty"`
	SessionCookies  string                                               `json:"session_cookies,omitempty"`
	Proxy           *browserProxyInput                                   `json:"proxy"`
	ExpectedIP      string                                               `json:"expected_ip"`
	CheckExitIP     func(context.Context) error                          `json:"-"`
	SkipExitIPCheck bool                                                 `json:"skip_exit_ip_check,omitempty"`
	ProxyExpiresAt  int64                                                `json:"proxy_expires_at"`
	Mode            string                                               `json:"mode"`
	URL             string                                               `json:"url"`
	Email           string                                               `json:"email"`
	Card            cardInput                                            `json:"card"`
	Billing         M                                                    `json:"billing"`
	PrepareSeconds  int                                                  `json:"prepare_seconds"`
	QuoteSeconds    int                                                  `json:"quote_seconds"`
	ResultSeconds   int                                                  `json:"result_seconds"`
	AutoConfirm     bool                                                 `json:"-"`
	MaxAmount       int64                                                `json:"-"`
	Currency        string                                               `json:"-"`
}
type paymentEvent struct {
	ProtocolLease string           `json:"-"`
	Phase         string           `json:"phase,omitempty"`
	HTTPStatus    int              `json:"http_status,omitempty"`
	RequestID     string           `json:"request_id,omitempty"`
	ElapsedMS     int64            `json:"elapsed_ms,omitempty"`
	Diagnostics   M                `json:"diagnostics,omitempty"`
	Evidence      *paymentEvidence `json:"evidence,omitempty"`
	Type          string           `json:"type"`
	State         string           `json:"state"`
	Code          string           `json:"code"`
	Sent          bool             `json:"sent"`
	Amount        int64            `json:"amount"`
	Currency      string           `json:"currency"`
}
type paymentEvidence struct {
	CheckoutID     string `json:"checkout_id"`
	IntentID       string `json:"payment_intent_id"`
	Status         string `json:"status"`
	AmountReceived *int64 `json:"amount_received"`
	Currency       string `json:"currency"`
}
type paymentCommand struct {
	Type string `json:"type"`
}
type paymentProcess interface {
	Events() <-chan paymentEvent
	Send(paymentCommand) error
	Close()
}
type paymentStarter func(context.Context, paymentInput) (paymentProcess, error)

func paymentRuntimePath() string {
	if p := os.Getenv("PAYMENT_RUNTIME_PATH"); p != "" {
		return p
	}
	return filepath.Join("runtime", "payment-browser.mjs")
}
func (a *App) paymentEnabled(ctx context.Context) bool {
	if a.config(ctx, "payment")["enabled"] != true {
		return false
	}
	if a.paymentStart != nil {
		return true
	}
	if _, err := exec.LookPath("node"); err != nil {
		return false
	}
	if _, err := os.Stat(paymentRuntimePath()); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(filepath.Dir(paymentRuntimePath()), "node_modules", "playwright", "package.json"))
	return err == nil
}

func (a *App) paymentExecutorEnabled(ctx context.Context, executor string) bool {
	if executor == paymentExecutorLegacyHTTP {
		return a.config(ctx, "payment")["enabled"] == true
	}
	return executor == paymentExecutorBrowser && a.paymentEnabled(ctx)
}

func (a *App) orderPaymentEnabled(ctx context.Context, id string) bool {
	var executor string
	err := a.DB.QueryRow(ctx, `SELECT CASE WHEN legacy_checkpoint_enc<>'' THEN 'legacy_http' ELSE executor END FROM payment_runs WHERE order_id=$1`, id).Scan(&executor)
	if err != nil {
		return a.paymentEnabled(ctx)
	}
	return a.paymentExecutorEnabled(ctx, executor)
}
