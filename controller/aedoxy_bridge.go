package controller

import (
	"errors"
	"net/http"
	"regexp"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// Machine API for the aedoxy.com storefront; errors are plain English, not console messages.

var (
	aedoxyTradeNoPattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	aedoxyUsernamePattern = regexp.MustCompile(`^[a-z0-9_]{3,20}$`)
)

type AedoxyEnsureAccountRequest struct {
	Subject     string `json:"subject"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Username    string `json:"username"`
}

type AedoxyAccountStatusRequest struct {
	Subject string `json:"subject"`
	Enabled *bool  `json:"enabled"`
}

type AedoxyFulfillRequest struct {
	TradeNo       string `json:"trade_no"`
	UserId        int    `json:"user_id"`
	PlanId        int    `json:"plan_id"`
	MoneyIdr      int64  `json:"money_idr"`
	PaymentMethod string `json:"payment_method"`
}

func AedoxyEnsureAccount(c *gin.Context) {
	var req AedoxyEnsureAccountRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	if n := utf8.RuneCountInString(req.Subject); n < 1 || n > 256 {
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("subject must be 1-256 characters"))
		return
	}

	if req.Username != "" && !aedoxyUsernamePattern.MatchString(req.Username) {
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("username must be 3-20 characters of a-z, 0-9 or _"))
		return
	}

	user, created, err := model.EnsureAedoxyUser(req.Subject, req.Email, req.DisplayName, req.Username)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, model.ErrAedoxyProviderMissing) {
			status = http.StatusServiceUnavailable
		}
		common.ApiErrorStatus(c, status, err)
		return
	}
	common.ApiSuccess(c, gin.H{
		"user_id":  user.Id,
		"username": user.Username,
		"created":  created,
	})
}

func AedoxyAccountStatus(c *gin.Context) {
	var req AedoxyAccountStatusRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	if n := utf8.RuneCountInString(req.Subject); n < 1 || n > 256 {
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("subject must be 1-256 characters"))
		return
	}
	if req.Enabled == nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("enabled is required"))
		return
	}

	user, changed, err := model.SetAedoxyUserEnabled(req.Subject, *req.Enabled)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, model.ErrAedoxyProviderMissing):
			status = http.StatusServiceUnavailable
		case errors.Is(err, model.ErrAedoxyUserNotFound):
			status = http.StatusNotFound
		case errors.Is(err, model.ErrAedoxyUserProtected):
			status = http.StatusConflict
		}
		common.ApiErrorStatus(c, status, err)
		return
	}
	if changed {
		action := "disable"
		if *req.Enabled {
			action = "enable"
		}
		recordManageAuditFor(c, user.Id, "user.manage", map[string]any{
			"action":   action,
			"username": user.Username,
			"id":       user.Id,
			"source":   "aedoxy_bridge",
		})
	}
	common.ApiSuccess(c, gin.H{
		"user_id": user.Id,
		"enabled": *req.Enabled,
		"changed": changed,
	})
}

func AedoxyFulfill(c *gin.Context) {
	if !requirePaymentCompliance(c) {
		return
	}

	var req AedoxyFulfillRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	switch {
	case !aedoxyTradeNoPattern.MatchString(req.TradeNo):
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("trade_no must match ^[A-Za-z0-9._:-]{1,64}$"))
		return
	case req.UserId <= 0 || req.PlanId <= 0:
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("user_id and plan_id must be positive"))
		return
	case req.MoneyIdr < 0 || req.MoneyIdr > 10000000:
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("money_idr must be between 0 and 10000000"))
		return
	case utf8.RuneCountInString(req.PaymentMethod) > 50:
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("payment_method must be at most 50 characters"))
		return
	}

	result, err := model.FulfillAedoxySubscription(req.TradeNo, req.UserId, req.PlanId, req.MoneyIdr, req.PaymentMethod)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, model.ErrAedoxyPlanNotFound), errors.Is(err, model.ErrAedoxyUserNotFound):
			status = http.StatusNotFound
		case errors.Is(err, model.ErrAedoxyOrderConflict), errors.Is(err, model.ErrSubscriptionOrderStatusInvalid):
			status = http.StatusConflict
		}
		common.ApiErrorStatus(c, status, err)
		return
	}
	common.ApiSuccess(c, result)
}

// aedoxyGetPricing is a variable only so tests can supply pricing without channels.
var aedoxyGetPricing = model.GetPricing

// AedoxyModelPrices returns the base input/output USD per 1M tokens of every
// expression-priced model; the storefront applies group ratios itself.
func AedoxyModelPrices(c *gin.Context) {
	common.ApiSuccess(c, gin.H{"models": model.AedoxyExpressionModelPrices(aedoxyGetPricing())})
}
