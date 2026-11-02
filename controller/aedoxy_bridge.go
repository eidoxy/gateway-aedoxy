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

var aedoxyTradeNoPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

type AedoxyEnsureAccountRequest struct {
	Subject     string `json:"subject"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
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

	user, created, err := model.EnsureAedoxyUser(req.Subject, req.Email, req.DisplayName)
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
