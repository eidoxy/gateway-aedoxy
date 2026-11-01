package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
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
// by email: a console email is not proof of owning the Aedoxy account.
func EnsureAedoxyUser(subject string, email string, displayName string) (*User, bool, error) {
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
		user.Username = "aedoxy_" + common.GetRandomString(aedoxyUsernameRandomLen)
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
		// Retry only a username collision; any other failure is final.
		if taken, checkErr := CheckUserExistOrDeleted(user.Username, ""); checkErr != nil || !taken {
			return nil, false, err
		}
	}
	return nil, false, err
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

		now := GetDBTimestamp()
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
