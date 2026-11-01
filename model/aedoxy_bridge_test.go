package model

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// useAedoxyTestDB swaps in a private database with a multi-connection pool: the
// shared single-connection test DB deadlocks on GetDBTimestamp, which reads
// through the global handle while a transaction holds the only connection.
// It is in-memory SQLite unless AEDOXY_TEST_DSN holds a mysql or postgres DSN
// in the SQL_DSN format; the tables are then dropped and recreated per test.
func useAedoxyTestDB(t *testing.T) {
	t.Helper()
	previousDB, previousLogDB := DB, LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	var db *gorm.DB
	var err error
	if os.Getenv("AEDOXY_TEST_DSN") != "" {
		var dbType common.DatabaseType
		db, dbType, err = chooseDB("AEDOXY_TEST_DSN", false)
		require.NoError(t, err)
		require.NotEqual(t, common.DatabaseTypeSQLite, dbType, "AEDOXY_TEST_DSN must be a mysql or postgres DSN")
		common.SetDatabaseTypes(dbType, dbType)
		initCol()
	} else {
		common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
		db, err = gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))), &gorm.Config{})
		require.NoError(t, err)
	}
	DB, LOG_DB = db, db
	tables := []any{&User{}, &Log{}, &TopUp{}, &SubscriptionPlan{}, &SubscriptionOrder{},
		&UserSubscription{}, &UserOAuthBinding{}, &CustomOAuthProvider{}}
	require.NoError(t, db.Migrator().DropTable(tables...))
	require.NoError(t, db.AutoMigrate(tables...))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	t.Cleanup(func() {
		DB, LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		initCol()
		_ = sqlDB.Close()
	})
}

func seedAedoxyProvider(t *testing.T) *CustomOAuthProvider {
	t.Helper()
	provider := &CustomOAuthProvider{Name: "Aedoxy", Slug: AedoxyOAuthSlug}
	require.NoError(t, DB.Create(provider).Error)
	return provider
}

func seedAedoxyUser(t *testing.T, username string, email string) *User {
	t.Helper()
	user := &User{Username: username, Email: email, Group: "default", AffCode: username}
	require.NoError(t, DB.Create(user).Error)
	return user
}

func seedAedoxyPlan(t *testing.T, title string, total int64, upgradeGroup string) *SubscriptionPlan {
	t.Helper()
	plan := &SubscriptionPlan{
		Title:         title,
		DurationUnit:  SubscriptionDurationDay,
		DurationValue: 30,
		TotalAmount:   total,
		UpgradeGroup:  upgradeGroup,
		Enabled:       true,
	}
	require.NoError(t, DB.Create(plan).Error)
	return plan
}

func countActiveSubscriptions(t *testing.T, userId int) int64 {
	t.Helper()
	var count int64
	require.NoError(t, DB.Model(&UserSubscription{}).Where("user_id = ? AND status = ?", userId, "active").Count(&count).Error)
	return count
}

func TestEnsureAedoxyUserCreatesUserAndBinding(t *testing.T) {
	useAedoxyTestDB(t)
	provider := seedAedoxyProvider(t)

	user, created, err := EnsureAedoxyUser("sub-1", "Buyer@Example.com", "Buyer")
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "buyer@example.com", user.Email)
	assert.LessOrEqual(t, len(user.Username), UserNameMaxLength)

	bound, err := GetUserByOAuthBinding(provider.Id, "sub-1")
	require.NoError(t, err)
	assert.Equal(t, user.Id, bound.Id)
}

func TestEnsureAedoxyUserIsIdempotentPerSubject(t *testing.T) {
	useAedoxyTestDB(t)
	seedAedoxyProvider(t)

	first, created, err := EnsureAedoxyUser("sub-1", "", "Buyer")
	require.NoError(t, err)
	require.True(t, created)

	second, created, err := EnsureAedoxyUser("sub-1", "", "Buyer")
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first.Id, second.Id)

	other, created, err := EnsureAedoxyUser("sub-2", "", "Buyer")
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotEqual(t, first.Id, other.Id)
}

func TestEnsureAedoxyUserNeverAttachesByEmail(t *testing.T) {
	useAedoxyTestDB(t)
	seedAedoxyProvider(t)
	existing := seedAedoxyUser(t, "existing", "shared@example.com")

	user, created, err := EnsureAedoxyUser("sub-1", "shared@example.com", "Buyer")
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotEqual(t, existing.Id, user.Id)
	assert.Empty(t, user.Email)
}

func TestEnsureAedoxyUserRequiresProvider(t *testing.T) {
	useAedoxyTestDB(t)

	_, _, err := EnsureAedoxyUser("sub-1", "", "Buyer")
	assert.ErrorIs(t, err, ErrAedoxyProviderMissing)
}

func TestFulfillAedoxySubscriptionIsIdempotentPerTradeNo(t *testing.T) {
	useAedoxyTestDB(t)
	user := seedAedoxyUser(t, "buyer", "")
	plan := seedAedoxyPlan(t, "Pro", 1000, "")

	first, err := FulfillAedoxySubscription("T-1", user.Id, plan.Id, 150000, "qris")
	require.NoError(t, err)
	assert.False(t, first.AlreadyFulfilled)
	assert.Empty(t, first.Replaced)

	retry, err := FulfillAedoxySubscription("T-1", user.Id, plan.Id, 150000, "qris")
	require.NoError(t, err)
	assert.True(t, retry.AlreadyFulfilled)
	assert.Equal(t, first.UserSubscriptionId, retry.UserSubscriptionId)
	assert.Equal(t, first.EndTime, retry.EndTime)
	assert.Equal(t, first.Replaced, retry.Replaced)

	var total int64
	require.NoError(t, DB.Model(&UserSubscription{}).Where("user_id = ?", user.Id).Count(&total).Error)
	assert.EqualValues(t, 1, total)
}

func TestFulfillAedoxySubscriptionReplacesActiveSubscription(t *testing.T) {
	useAedoxyTestDB(t)
	user := seedAedoxyUser(t, "buyer", "")
	oldPlan := seedAedoxyPlan(t, "Pro", 1000, "vip")
	newPlan := seedAedoxyPlan(t, "Max", 5000, "")

	first, err := FulfillAedoxySubscription("T-1", user.Id, oldPlan.Id, 100000, "qris")
	require.NoError(t, err)
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", first.UserSubscriptionId).Update("amount_used", 300).Error)

	second, err := FulfillAedoxySubscription("T-2", user.Id, newPlan.Id, 200000, "qris")
	require.NoError(t, err)
	assert.Equal(t, []AedoxyReplacedSubscription{{Id: first.UserSubscriptionId, Remaining: 700}}, second.Replaced)
	assert.EqualValues(t, 1, countActiveSubscriptions(t, user.Id))

	var old UserSubscription
	require.NoError(t, DB.First(&old, first.UserSubscriptionId).Error)
	assert.Equal(t, "cancelled", old.Status)

	// The cancelled plan's group upgrade is rolled back by the shared downgrade logic.
	var reloaded User
	require.NoError(t, DB.First(&reloaded, user.Id).Error)
	assert.Equal(t, "default", reloaded.Group)
}

func TestFulfillAedoxySubscriptionRejectsTradeNoReuse(t *testing.T) {
	useAedoxyTestDB(t)
	buyer := seedAedoxyUser(t, "buyer", "")
	other := seedAedoxyUser(t, "other", "")
	plan := seedAedoxyPlan(t, "Pro", 1000, "")

	_, err := FulfillAedoxySubscription("T-1", buyer.Id, plan.Id, 100000, "qris")
	require.NoError(t, err)

	_, err = FulfillAedoxySubscription("T-1", other.Id, plan.Id, 100000, "qris")
	assert.ErrorIs(t, err, ErrAedoxyOrderConflict)
	assert.EqualValues(t, 0, countActiveSubscriptions(t, other.Id))

	_, err = FulfillAedoxySubscription("T-9", 999999, plan.Id, 100000, "qris")
	assert.ErrorIs(t, err, ErrAedoxyUserNotFound)
	_, err = FulfillAedoxySubscription("T-9", buyer.Id, 999999, 100000, "qris")
	assert.ErrorIs(t, err, ErrAedoxyPlanNotFound)
}

func TestEnsureAedoxyUserConcurrentSameSubject(t *testing.T) {
	useAedoxyTestDB(t)
	provider := seedAedoxyProvider(t)

	const workers = 8
	ids := make([]int, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user, _, err := EnsureAedoxyUser("sub-race", "", "Buyer")
			errs[i] = err
			if user != nil {
				ids[i] = user.Id
			}
		}()
	}
	wg.Wait()
	for i := range workers {
		require.NoError(t, errs[i])
		assert.Equal(t, ids[0], ids[i])
	}
	var users, bindings int64
	require.NoError(t, DB.Model(&User{}).Count(&users).Error)
	require.NoError(t, DB.Model(&UserOAuthBinding{}).Where("provider_id = ?", provider.Id).Count(&bindings).Error)
	assert.EqualValues(t, 1, users)
	assert.EqualValues(t, 1, bindings)
}

func TestFulfillAedoxySubscriptionConcurrentSameTradeNo(t *testing.T) {
	useAedoxyTestDB(t)
	user := seedAedoxyUser(t, "buyer", "")
	plan := seedAedoxyPlan(t, "Pro", 1000, "")

	// shortcut: CreateUserSubscriptionFromPlanTx still reads time via the global DB, so workers must stay below the pool size (8) or a saturated pool deadlocks; revisit if upstream passes tx.
	const workers = 4
	results := make([]*AedoxyFulfillResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = FulfillAedoxySubscription("T-race", user.Id, plan.Id, 150000, "qris")
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent fulfill did not finish in 30s (pool or lock deadlock)")
	}
	fresh := 0
	for i := range workers {
		require.NoError(t, errs[i])
		assert.Equal(t, results[0].UserSubscriptionId, results[i].UserSubscriptionId)
		if !results[i].AlreadyFulfilled {
			fresh++
		}
	}
	assert.Equal(t, 1, fresh)
	var subs, orders int64
	require.NoError(t, DB.Model(&UserSubscription{}).Where("user_id = ?", user.Id).Count(&subs).Error)
	require.NoError(t, DB.Model(&SubscriptionOrder{}).Where("trade_no = ?", "T-race").Count(&orders).Error)
	assert.EqualValues(t, 1, subs)
	assert.EqualValues(t, 1, orders)
}
