package platform

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"regexp"
	"strings"
)

type legacyPaymentCheckpoint struct {
	CheckoutID string `json:"checkout_id"`
	PageID     string `json:"page_id,omitempty"`
	Key        string `json:"key"`
	EID        string `json:"eid,omitempty"`
	IntentID   string `json:"intent_id,omitempty"`
	Secret     string `json:"secret,omitempty"`
}

type legacyPaymentSnapshot struct {
	Amount                                              int64
	Currency                                            string
	TaxReady, Open, Paid, Action, Authorized, Attempted bool
	Evidence                                            *paymentEvidence
}

var legacyPublishableKey = regexp.MustCompile(`^pk_(live|test)_[A-Za-z0-9]+$`)
var legacySecret = regexp.MustCompile(`^(pi_[A-Za-z0-9]+)_secret_[A-Za-z0-9]+$`)

func legacyMap(v any) M {
	switch m := v.(type) {
	case M:
		return m
	}
	return nil
}
func legacyMinor(v any) (int64, bool) {
	var n int64
	switch x := v.(type) {
	case json.Number:
		var e error
		n, e = x.Int64()
		if e != nil {
			return 0, false
		}
	case int64:
		n = x
	case int:
		n = int64(x)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || x != math.Trunc(x) || x > 10000000000 {
			return 0, false
		}
		n = int64(x)
	default:
		return 0, false
	}
	return n, n > 0 && n <= 10000000000
}
func legacyCurrency(v string) bool {
	return strings.Contains("|usd|php|jpy|sgd|gbp|egp|try|", "|"+v+"|") && v != ""
}

func legacyURLKey(raw string) string {
	u, e := url.Parse(raw)
	if e != nil {
		return ""
	}
	encoded := u.Fragment
	if decoded, err := url.QueryUnescape(encoded); err == nil {
		encoded = decoded
	}
	encoded += strings.Repeat("=", (4-len(encoded)%4)%4)
	b, e := base64.StdEncoding.DecodeString(encoded)
	if e != nil {
		return ""
	}
	for i := range b {
		b[i] ^= 5
	}
	var data M
	if json.Unmarshal(b, &data) != nil {
		return ""
	}
	for _, k := range []string{"apiKey", "publishableKey", "publishable_key"} {
		if legacyPublishableKey.MatchString(str(data, k)) {
			return str(data, k)
		}
	}
	return ""
}

func legacyPageSnapshot(data M, id string) (*legacyPaymentSnapshot, error) {
	if str(data, "object") != "checkout.session" || str(data, "session_id") != id || !regexp.MustCompile(`^ppage_[A-Za-z0-9]+$`).MatchString(str(data, "id")) {
		return nil, errors.New("invalid_response")
	}
	amount, ok := legacyMinor(legacyMap(data["total_summary"])["due"])
	currency := str(data, "currency")
	if !ok || !legacyCurrency(currency) {
		return nil, errors.New("quote_unavailable")
	}
	s := &legacyPaymentSnapshot{Amount: amount, Currency: currency}
	intent := legacyMap(data["payment_intent"])
	intentID := str(intent, "id")
	if intent != nil && (!paymentIntentID.MatchString(intentID) || str(intent, "currency") != currency) {
		return nil, errors.New("invalid_response")
	}
	status := str(intent, "status")
	var received *int64
	if status == "succeeded" {
		if v, exists := intent["amount_received"]; exists && v != nil {
			n, valid := legacyMinor(v)
			if !valid || n != amount {
				return nil, errors.New("amount_changed")
			}
			received = &n
		}
		if v, exists := intent["amount"]; exists {
			n, valid := legacyMinor(v)
			if !valid || n != amount {
				return nil, errors.New("amount_changed")
			}
		}
	}
	s.Paid = received != nil || (str(data, "payment_status") == "paid" && str(data, "status") == "complete" && str(data, "state") == "succeeded" && (intent == nil || status == "succeeded"))
	s.TaxReady = legacyMap(data["tax_context"])["automatic_tax_enabled"] == false || str(legacyMap(data["tax_meta"]), "status") == "complete"
	sub := legacyMap(data["submission_attempt"])
	s.Action = status == "requires_action" || str(sub, "state") == "requires_approval" || str(sub, "status") == "requires_approval" || str(sub, "state") == "requires_action" || str(sub, "status") == "requires_action"
	s.Authorized = status == "requires_capture"
	s.Attempted = data["submission_attempt"] != nil || data["payment_intent"] != nil
	s.Open = str(data, "status") == "open" && str(data, "payment_status") == "unpaid"
	s.Evidence = &paymentEvidence{CheckoutID: id, IntentID: intentID, Status: "pending", AmountReceived: received, Currency: currency}
	if s.Paid {
		s.Evidence.Status = "succeeded"
	} else if s.Action {
		s.Evidence.Status = "action_required"
	} else if s.Authorized {
		s.Evidence.Status = "authorized"
	}
	return s, nil
}

func legacyIntentSnapshot(data M, cp legacyPaymentCheckpoint) (*legacyPaymentSnapshot, error) {
	if str(data, "object") != "payment_intent" || str(data, "id") != cp.IntentID || !paymentIntentID.MatchString(cp.IntentID) {
		return nil, errors.New("invalid_response")
	}
	n, ok := legacyMinor(data["amount"])
	currency := str(data, "currency")
	if !ok || !legacyCurrency(currency) {
		return nil, errors.New("invalid_response")
	}
	s := &legacyPaymentSnapshot{Amount: n, Currency: currency, TaxReady: true, Attempted: true}
	s.Action = str(data, "status") == "requires_action"
	s.Authorized = str(data, "status") == "requires_capture"
	s.Evidence = &paymentEvidence{CheckoutID: cp.CheckoutID, IntentID: cp.IntentID, Status: "pending", Currency: currency}
	if str(data, "status") == "succeeded" {
		received, valid := legacyMinor(data["amount_received"])
		if !valid || received != n {
			return nil, errors.New("amount_changed")
		}
		s.Paid = true
		s.Evidence.Status = "succeeded"
		s.Evidence.AmountReceived = &received
	} else if s.Action {
		s.Evidence.Status = "action_required"
	} else if s.Authorized {
		s.Evidence.Status = "authorized"
	}
	return s, nil
}
