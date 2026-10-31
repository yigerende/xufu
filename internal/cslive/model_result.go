// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	json "encoding/json"
	fmt "fmt"
)

type model_PaymentState string

const (
	model_PaymentNotAttempted   model_PaymentState = "not_attempted"
	model_PaymentUnknown        model_PaymentState = "unknown"
	model_PaymentPending        model_PaymentState = "pending"
	model_PaymentActionRequired model_PaymentState = "action_required"
	model_PaymentAuthorized     model_PaymentState = "authorized"
	model_PaymentCharged        model_PaymentState = "charged"
	model_PaymentDeclined       model_PaymentState = "not_charged"
)

type model_CheckoutPrice struct {
	Currency      string `json:"currency"`
	SubtotalMinor *int64 `json:"subtotal_minor"`
	TotalMinor    *int64 `json:"total_minor"`
	TaxMinor      *int64 `json:"tax_minor"`
	DiscountMinor *int64 `json:"discount_minor"`
}

type model_CheckoutResult struct {
	CheckoutID      string               `json:"checkout_id"`
	CheckoutURL     string               `json:"checkout_url"`
	ProcessorEntity string               `json:"processor_entity"`
	ErrorCode       string               `json:"error_code"`
	Price           *model_CheckoutPrice `json:"price"`
	Flow            string               `json:"flow,omitempty"`
}

func (r model_CheckoutResult) String() string {
	return fmt.Sprintf("CheckoutResult{CheckoutID:%s ErrorCode:%s Flow:%s URL:redacted}", r.CheckoutID, r.ErrorCode, r.Flow)
}

func (r model_CheckoutResult) GoString() string { return r.String() }

type model_Result struct {
	ProviderCode, DeclineCode                        string
	Payment                                          model_PaymentState
	Activation, Renewal, CheckoutID, PaymentIntentID string
	Amount                                           *int64
	Currency, ErrorCode                              string
	WarningCode                                      string
	Flow                                             string
	PaymentElapsedMS                                 *int64
	EstimatedPrice, ChargedPrice                     *model_Price
	Checkpoint                                       model_Checkpoint
}

func (r model_Result) Public() protocol_Map {
	result := protocol_Map{"payment": string(r.Payment), "activation": r.Activation, "renewal": r.Renewal, "checkout_id": r.CheckoutID, "payment_intent_id": r.PaymentIntentID, "amount": r.Amount, "currency": r.Currency, "error_code": r.ErrorCode, "phase": r.Checkpoint.Phase}
	if r.Payment == model_PaymentDeclined {
		if r.ProviderCode != "" {
			result["provider_code"] = r.ProviderCode
		}
		if r.DeclineCode != "" {
			result["decline_code"] = r.DeclineCode
		}
		if reason := r.FailureReason(); reason != "" {
			result["failure_reason"] = reason
		}
	}
	if r.WarningCode != "" {
		result["warning_code"] = r.WarningCode
	}
	if r.Flow != "" {
		result["flow"] = r.Flow
	}
	return result
}

// JSON 输出始终使用公开字段；恢复凭据只通过 Checkpoint 显式持久化。
func (r model_Result) MarshalJSON() ([]byte, error) { return json.Marshal(r.Public()) }

func (r model_Result) String() string { s, _ := protocol_CompactJSON(r.Public()); return s }

func (r model_Result) GoString() string { return r.String() }

type model_PaymentApproval func(int64, string) (bool, error)

// FailureReason 使用固定文案，不显示上游自由文本。
func (r model_Result) FailureReason() string {
	if r.Payment != model_PaymentDeclined {
		return ""
	}
	if r.DeclineCode == "card_velocity_exceeded" {
		return "卡片可用余额、信用额度或交易金额限制不足，请向发卡方核实"
	}
	if r.ProviderCode == "card_declined" {
		return "付款被拒绝，请结合 decline_code 向发卡方核实"
	}
	if r.ProviderCode == "payment_intent_authentication_failure" {
		return "付款验证失败"
	}
	return ""
}
