package model

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The Aedoxy bridge lets the aedoxy.com storefront create console accounts and
// activate paid plans. The storefront owns orders and payment; this side only
// turns a paid order into exactly one active subscription.

const (
	PaymentProviderAedoxy = "aedoxy"
	AedoxyOAuthSlug       = "aedoxy"

	// users.username allows 20 chars: "aedoxy_" (7) + 13 random chars.
	aedoxyUsernameRandomLen = UserNameMaxLength - len("aedoxy_")
	aedoxyCreateAttempts    = 3
)

var (
	ErrAedoxyProviderMissing = errors.New("aedoxy oauth provider is not configured")
	ErrAedoxyOrderConflict   = errors.New("trade_no belongs to a different user, plan or provider")
	ErrAedoxyPlanNotFound    = errors.New("subscription plan not found")
	ErrAedoxyUserNotFound    = errors.New("user not found")
	ErrAedoxyUserProtected   = errors.New("administrator accounts cannot be disabled through the bridge")
)

type AedoxyReplacedSubscription struct {
	Id        int   `json:"id"`
	Remaining int64 `json:"remaining"`
}

type AedoxyFulfillResult struct {
	AlreadyFulfilled   bool                         `json:"already_fulfilled"`
	UserSubscriptionId int                          `json:"user_subscription_id"`
	EndTime            int64                        `json:"end_time"`
	Replaced           []AedoxyReplacedSubscription `json:"replaced"`
}

// EnsureAedoxyUser returns the console user bound to an Aedoxy account subject,
// creating the user and its OAuth binding on first use. Users are never matched
// by email: a console email is not proof of owning the Aedoxy account. A
// non-empty username is tried first for a new user; if it is taken, a random
// one is used. An existing binding keeps its user and username unchanged.
func EnsureAedoxyUser(subject string, email string, displayName string, username string) (*User, bool, error) {
	provider, err := GetCustomOAuthProviderBySlug(AedoxyOAuthSlug)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, ErrAedoxyProviderMissing
		}
		return nil, false, err
	}

	existing, err := GetUserByOAuthBinding(provider.Id, subject)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	display := []rune(strings.TrimSpace(displayName))
	if len(display) > UserNameMaxLength {
		display = display[:UserNameMaxLength]
	}
	user := &User{
		DisplayName: string(display),
		Email:       NormalizeEmail(email),
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
	}
	if user.DisplayName == "" {
		user.DisplayName = provider.Name + " User"
	}
	// users.email is a free-form column; an address that does not fit is not stored.
	if len(user.Email) > 50 {
		user.Email = ""
	}

	for range aedoxyCreateAttempts {
		if username == "" {
			username = "aedoxy_" + common.GetRandomString(aedoxyUsernameRandomLen)
		}
		user.Username = username
		err = DB.Transaction(func(tx *gorm.DB) error {
			if err := user.InsertWithTx(tx, 0); err != nil {
				return err
			}
			return CreateUserOAuthBindingWithTx(tx, &UserOAuthBinding{
				UserId:         user.Id,
				ProviderId:     provider.Id,
				ProviderUserId: subject,
			})
		})
		if err == nil {
			user.FinalizeOAuthUserCreation(0)
			return user, true, nil
		}
		// A concurrent call for the same subject won the unique binding index.
		if winner, readErr := GetUserByOAuthBinding(provider.Id, subject); readErr == nil {
			return winner, false, nil
		}
		// The address is optional here: lose it rather than the account.
		if errors.Is(err, ErrEmailAlreadyTaken) {
			user.Email = ""
			continue
		}
		// Retry only a username collision, with a random name; any other failure is final.
		if taken, checkErr := CheckUserExistOrDeleted(user.Username, ""); checkErr != nil || !taken {
			return nil, false, err
		}
		username = ""
	}
	return nil, false, err
}

// SetAedoxyUserEnabled enables or disables the console user bound to an Aedoxy
// account subject. It goes through User.Update like the admin path, which bumps
// the auth version, refreshes the user cache and revokes browser sessions. It is
// idempotent: changed is false when the user already has that status.
func SetAedoxyUserEnabled(subject string, enabled bool) (*User, bool, error) {
	provider, err := GetCustomOAuthProviderBySlug(AedoxyOAuthSlug)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, ErrAedoxyProviderMissing
		}
		return nil, false, err
	}
	user, err := GetUserByOAuthBinding(provider.Id, subject)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, ErrAedoxyUserNotFound
		}
		return nil, false, err
	}

	status := common.UserStatusDisabled
	if enabled {
		status = common.UserStatusEnabled
	}
	if user.Status == status {
		return user, false, nil
	}
	if !enabled && user.Role >= common.RoleAdminUser {
		return nil, false, ErrAedoxyUserProtected
	}

	// Only the status is set: Updates(struct) skips zero fields.
	updated := &User{Id: user.Id, Status: status}
	if err := updated.Update(false); err != nil {
		return nil, false, err
	}
	if err := InvalidateUserTokensCache(user.Id); err != nil {
		common.SysLog(common.LogText("failed to invalidate tokens cache for user %d: %s", user.Id, err.Error()))
	}
	return updated, true, nil
}

// FulfillAedoxySubscription activates the plan of a paid storefront order. It is
// idempotent per trade_no: a repeated call returns the first result. A user holds
// at most one active plan, so any active subscription is cancelled first and its
// unused amount is reported back for the storefront's credit pool.
func FulfillAedoxySubscription(tradeNo string, userId int, planId int, moneyIdr int64, paymentMethod string) (*AedoxyFulfillResult, error) {
	plan, err := GetSubscriptionPlanById(planId)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAedoxyPlanNotFound
		}
		return nil, err
	}

	refCol := "`trade_no`"
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		refCol = `"trade_no"`
	}

	result := &AedoxyFulfillResult{Replaced: []AedoxyReplacedSubscription{}}
	err = DB.Transaction(func(tx *gorm.DB) error {
		// The user row lock comes first so concurrent orders of one user, and
		// retries of one order, run one after another.
		var userRow User
		if err := lockForUpdate(tx).Select("id").Where("id = ?", userId).First(&userRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAedoxyUserNotFound
			}
			return err
		}

		// Created in the same transaction so a failed fulfilment leaves no order behind.
		seed := &SubscriptionOrder{
			UserId:          userId,
			PlanId:          planId,
			Money:           float64(moneyIdr),
			TradeNo:         tradeNo,
			PaymentMethod:   paymentMethod,
			PaymentProvider: PaymentProviderAedoxy,
			Status:          common.TopUpStatusPending,
			CreateTime:      common.GetTimestamp(),
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(seed).Error; err != nil {
			return err
		}

		var order SubscriptionOrder
		if err := lockForUpdate(tx).Where(refCol+" = ?", tradeNo).First(&order).Error; err != nil {
			return ErrSubscriptionOrderNotFound
		}
		if order.PaymentProvider != PaymentProviderAedoxy || order.UserId != userId || order.PlanId != planId {
			return ErrAedoxyOrderConflict
		}
		if order.Status == common.TopUpStatusSuccess {
			if err := common.UnmarshalJsonStr(order.ProviderPayload, result); err != nil {
				return fmt.Errorf("stored aedoxy result is unreadable: %w", err)
			}
			result.AlreadyFulfilled = true
			return nil
		}
		if order.Status != common.TopUpStatusPending {
			return ErrSubscriptionOrderStatusInvalid
		}

		now := dbTimestampTx(tx)
		var active []UserSubscription
		if err := tx.Where("user_id = ? AND status = ? AND end_time > ?", userId, "active", now).
			Order("id asc").Find(&active).Error; err != nil {
			return err
		}
		for i := range active {
			sub := &active[i]
			// shortcut: usage still in flight on the old plan settles after this snapshot, so the reported remainder can be slightly high; upgrade if the storefront needs exact credit.
			remaining := max(sub.AmountTotal-sub.AmountUsed, 0)
			if err := tx.Model(sub).Updates(map[string]any{
				"status":   "cancelled",
				"end_time": now,
			}).Error; err != nil {
				return err
			}
			if _, err := downgradeUserGroupForSubscriptionTx(tx, sub, now); err != nil {
				return err
			}
			result.Replaced = append(result.Replaced, AedoxyReplacedSubscription{Id: sub.Id, Remaining: remaining})
		}

		subscription, err := CreateUserSubscriptionFromPlanTx(tx, userId, plan, "order")
		if err != nil {
			return err
		}
		result.UserSubscriptionId = subscription.Id
		result.EndTime = subscription.EndTime

		if err := upsertSubscriptionTopUpTx(tx, &order); err != nil {
			return err
		}
		payload, err := common.Marshal(result)
		if err != nil {
			return err
		}
		order.Status = common.TopUpStatusSuccess
		order.CompleteTime = common.GetTimestamp()
		order.ProviderPayload = string(payload)
		return tx.Save(&order).Error
	})
	if err != nil {
		return nil, err
	}
	if result.AlreadyFulfilled {
		return result, nil
	}

	// Cancelling a plan or starting one can change the user's group.
	refreshSubscriptionUserGroupCache(userId, common.LogText("aedoxy plan activation"))
	RecordLog(userId, LogTypeTopup, common.NewMessage("Subscription purchased, plan: {{plan}}, payment amount: {{amount}}, payment method: {{method}}", map[string]any{
		"plan":   plan.Title,
		"amount": fmt.Sprintf("Rp %d", moneyIdr),
		"method": paymentMethod,
	}))
	return result, nil
}

// dbTimestampTx is GetDBTimestamp on the transaction handle, so a tx holding row locks never needs a second pooled connection.
func dbTimestampTx(tx *gorm.DB) int64 {
	var ts int64
	var err error
	switch {
	case common.UsingMainDatabase(common.DatabaseTypePostgreSQL):
		err = tx.Raw("SELECT EXTRACT(EPOCH FROM NOW())::bigint").Scan(&ts).Error
	case common.UsingMainDatabase(common.DatabaseTypeSQLite):
		err = tx.Raw("SELECT strftime('%s','now')").Scan(&ts).Error
	default:
		err = tx.Raw("SELECT UNIX_TIMESTAMP()").Scan(&ts).Error
	}
	if err != nil || ts <= 0 {
		return common.GetTimestamp()
	}
	return ts
}

// AedoxyModelPrice is the base input/output price of an expression-priced model
// in USD per 1M tokens, before any group ratio.
type AedoxyModelPrice struct {
	ModelName     string  `json:"model_name"`
	BillingMode   string  `json:"billing_mode"`
	InputUsdPerM  float64 `json:"input_usd_per_m"`
	OutputUsdPerM float64 `json:"output_usd_per_m"`
	Dynamic       bool    `json:"dynamic"`
}

// aedoxyPriceProbeTokens keeps `len` far below any context-tier threshold, so
// tier conditions pick the short-context (base) price.
const aedoxyPriceProbeTokens = 1000

var (
	aedoxyTimeFuncPattern  = regexp.MustCompile(`\b(?:hour|minute|weekday|month|day)\s*\(`)
	aedoxyStringLiteralPat = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`)
)

// AedoxyExpressionModelPrices prices every expression-billed model with the
// billing engine itself. Expression coefficients are USD per 1M tokens and the
// result is linear in token counts, so result / probe tokens equals the
// coefficient. The input slice is shared between goroutines and is not modified.
// Expressions that cannot be priced as plain per-token rates (fixed per-request
// prices, compile or run errors, non-finite or negative results) are skipped.
// For time-dependent expressions the engine reads the current clock, so the
// price is the one in effect at call time (Dynamic is true).
func AedoxyExpressionModelPrices(pricing []Pricing) []AedoxyModelPrice {
	prices := make([]AedoxyModelPrice, 0, len(pricing))
	for _, entry := range pricing {
		if entry.BillingMode != billing_setting.BillingModeTieredExpr || entry.BillingExpr == "" || billingexpr.UsesFixedPricing(entry.BillingExpr) {
			continue
		}
		in, _, inErr := billingexpr.RunExpr(entry.BillingExpr, billingexpr.TokenParams{P: aedoxyPriceProbeTokens, Len: aedoxyPriceProbeTokens})
		out, _, outErr := billingexpr.RunExpr(entry.BillingExpr, billingexpr.TokenParams{C: aedoxyPriceProbeTokens, Len: aedoxyPriceProbeTokens})
		if inErr != nil || outErr != nil {
			continue
		}
		in, out = math.Round(in/aedoxyPriceProbeTokens*1e9)/1e9, math.Round(out/aedoxyPriceProbeTokens*1e9)/1e9
		if math.IsNaN(in) || math.IsInf(in, 0) || in < 0 || math.IsNaN(out) || math.IsInf(out, 0) || out < 0 {
			continue
		}
		prices = append(prices, AedoxyModelPrice{
			ModelName:     entry.ModelName,
			BillingMode:   entry.BillingMode,
			InputUsdPerM:  in,
			OutputUsdPerM: out,
			Dynamic:       aedoxyTimeFuncPattern.MatchString(aedoxyStringLiteralPat.ReplaceAllString(entry.BillingExpr, `""`)),
		})
	}
	slices.SortFunc(prices, func(a, b AedoxyModelPrice) int { return strings.Compare(a.ModelName, b.ModelName) })
	return prices
}
