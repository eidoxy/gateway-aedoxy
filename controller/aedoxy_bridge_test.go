package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type aedoxyBridgeResponse struct {
	Success bool           `json:"success"`
	Message any            `json:"message"`
	Data    map[string]any `json:"data"`
}

// callAedoxyBridge runs a bridge handler as the storefront's token user would.
func callAedoxyBridge(t *testing.T, handler gin.HandlerFunc, body string) (int, aedoxyBridgeResponse) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/aedoxy", strings.NewReader(body))
	c.Set("id", 1)
	c.Set("role", common.RoleRootUser)
	handler(c)
	var response aedoxyBridgeResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	return recorder.Code, response
}

func TestAedoxyBridgeUsernameAndStatus(t *testing.T) {
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.CustomOAuthProvider{}, &model.UserOAuthBinding{}))
	provider := &model.CustomOAuthProvider{Name: "Aedoxy", Slug: model.AedoxyOAuthSlug}
	require.NoError(t, db.Create(provider).Error)

	for _, username := range []string{"ab", "Upper", "has-dash", "this_name_is_way_too_long"} {
		code, response := callAedoxyBridge(t, AedoxyEnsureAccount, `{"subject":"s-bad","username":"`+username+`"}`)
		assert.Equal(t, http.StatusBadRequest, code, username)
		assert.False(t, response.Success, username)
	}

	code, response := callAedoxyBridge(t, AedoxyEnsureAccount, `{"subject":"s-1","display_name":"Buyer","username":"buyer_one"}`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "buyer_one", response.Data["username"])
	assert.Equal(t, true, response.Data["created"])
	userId := int(response.Data["user_id"].(float64))

	_, response = callAedoxyBridge(t, AedoxyEnsureAccount, `{"subject":"s-1","display_name":"Buyer","username":"changed"}`)
	assert.Equal(t, "buyer_one", response.Data["username"])
	assert.Equal(t, false, response.Data["created"])

	code, _ = callAedoxyBridge(t, AedoxyAccountStatus, `{"subject":"s-1"}`)
	assert.Equal(t, http.StatusBadRequest, code, "enabled is required")
	code, _ = callAedoxyBridge(t, AedoxyAccountStatus, `{"subject":"missing","enabled":false}`)
	assert.Equal(t, http.StatusNotFound, code)

	code, response = callAedoxyBridge(t, AedoxyAccountStatus, `{"subject":"s-1","enabled":false}`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, map[string]any{"user_id": float64(userId), "enabled": false, "changed": true}, response.Data)
	_, response = callAedoxyBridge(t, AedoxyAccountStatus, `{"subject":"s-1","enabled":false}`)
	assert.Equal(t, false, response.Data["changed"])

	// Contract check: the OIDC login of a disabled user finds the account even
	// with registration off, and handleOAuthLogin then refuses it.
	previousRegister := common.RegisterEnabled
	common.RegisterEnabled = false
	t.Cleanup(func() { common.RegisterEnabled = previousRegister })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/oauth/aedoxy", nil)
	handleOAuthLogin(c, oauth.NewGenericOAuthProvider(provider), &oauth.OAuthUser{ProviderUserID: "s-1"}, &oauth.OAuthToken{}, &model.AuthFlow{Payload: "{}"})
	assert.Contains(t, recorder.Body.String(), `"success":false`)
	assert.Contains(t, recorder.Body.String(), "banned")

	// Administrators cannot be locked out through the bridge.
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", userId).Updates(map[string]any{"role": common.RoleAdminUser, "status": common.UserStatusEnabled}).Error)
	code, _ = callAedoxyBridge(t, AedoxyAccountStatus, `{"subject":"s-1","enabled":false}`)
	assert.Equal(t, http.StatusConflict, code)
}

// setupBoundAedoxyRoot makes the fixture user a root bound to the Aedoxy provider as subject "sub-root".
func setupBoundAedoxyRoot(t *testing.T) (*model.User, *model.CustomOAuthProvider) {
	t.Helper()
	root, _ := setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.CustomOAuthProvider{}))
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", root.Id).Update("role", common.RoleRootUser).Error)
	provider := &model.CustomOAuthProvider{Name: "Aedoxy", Slug: model.AedoxyOAuthSlug}
	require.NoError(t, model.DB.Create(provider).Error)
	require.NoError(t, model.CreateUserOAuthBinding(&model.UserOAuthBinding{UserId: root.Id, ProviderId: provider.Id, ProviderUserId: "sub-root"}))
	return root, provider
}

func loginViaAedoxy(provider *model.CustomOAuthProvider) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/oauth/aedoxy", nil)
	handleOAuthLogin(c, oauth.NewGenericOAuthProvider(provider), &oauth.OAuthUser{ProviderUserID: "sub-root"}, &oauth.OAuthToken{}, &model.AuthFlow{Payload: "{}"})
	return recorder
}

// TestAedoxyRootPasskeyBecomesLoginFactor documents a runbook warning: a root with
// a passkey must pass it on every login, Aedoxy included, so it has to stay enabled.
func TestAedoxyRootPasskeyBecomesLoginFactor(t *testing.T) {
	root, provider := setupBoundAedoxyRoot(t)
	newSecurityLoginPasskey(t, root.Id)

	assert.Contains(t, loginViaAedoxy(provider).Body.String(), `"require_verification":true`)

	settings := system_setting.GetPasskeySettings()
	settings.Enabled = false
	blocked := loginViaAedoxy(provider).Body.String()
	assert.Contains(t, blocked, `"success":false`)
	assert.NotContains(t, blocked, `"access_token"`)

	// Runbook recovery for a lost device: the factor state is read from the tables on every login.
	require.NoError(t, model.DB.Exec(`DELETE FROM passkey_credentials WHERE user_id = ?`, root.Id).Error)
	assert.Contains(t, loginViaAedoxy(provider).Body.String(), `"access_token"`)

	require.NoError(t, model.DB.Create(&model.TwoFA{UserId: root.Id, Secret: "JBSWY3DPEHPK3PXP", IsEnabled: true}).Error)
	assert.Contains(t, loginViaAedoxy(provider).Body.String(), `"require_verification":true`)
	require.NoError(t, model.DB.Exec(`UPDATE two_fas SET is_enabled = false WHERE user_id = ?`, root.Id).Error)
	assert.Contains(t, loginViaAedoxy(provider).Body.String(), `"access_token"`)
}

// TestAedoxyRootSurvivesPasswordLoginDisabled covers the console runbook: with
// password login and registration switched off, the bound root still enters
// through the Aedoxy provider, and the options table is the way back.
func TestAedoxyRootSurvivesPasswordLoginDisabled(t *testing.T) {
	root, provider := setupBoundAedoxyRoot(t)

	previousLogin, previousPasswordRegister, previousRegister := common.PasswordLoginEnabled, common.PasswordRegisterEnabled, common.RegisterEnabled
	common.OptionMapRWMutex.Lock()
	previousOptionMap := common.OptionMap
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.PasswordLoginEnabled, common.PasswordRegisterEnabled, common.RegisterEnabled = previousLogin, previousPasswordRegister, previousRegister
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptionMap
		common.OptionMapRWMutex.Unlock()
	})

	passwordLogin := func() *httptest.ResponseRecorder {
		return securityEnrollmentRequest("POST", "/api/user/login", `{"username":"enrollment-user","password":"enrollment-password"}`, "", service.AuthIdentity{}, Login)
	}
	aedoxyLogin := func() *httptest.ResponseRecorder { return loginViaAedoxy(provider) }

	// The owner switches both off in the console, which stores them in options.
	for _, option := range []model.Option{{Key: "PasswordLoginEnabled", Value: "false"}, {Key: "PasswordRegisterEnabled", Value: "false"}} {
		require.NoError(t, model.DB.Create(&option).Error)
	}
	model.InitOptionMap()
	require.False(t, common.PasswordLoginEnabled)
	require.False(t, common.PasswordRegisterEnabled)

	denied := passwordLogin()
	assert.Contains(t, denied.Body.String(), `"success":false`)
	assert.Contains(t, denied.Body.String(), "Password login has been disabled by administrator")

	for _, registerEnabled := range []bool{true, false} {
		common.RegisterEnabled = registerEnabled
		recorder := aedoxyLogin()
		var response struct {
			Success bool `json:"success"`
			Data    struct {
				AccessToken string `json:"access_token"`
				User        struct {
					Id   int `json:"id"`
					Role int `json:"role"`
				} `json:"user"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
		assert.True(t, response.Success, "register=%v: %s", registerEnabled, recorder.Body.String())
		assert.NotEmpty(t, response.Data.AccessToken)
		assert.Equal(t, root.Id, response.Data.User.Id)
		assert.Equal(t, common.RoleRootUser, response.Data.User.Role)
	}
	var users int64
	require.NoError(t, model.DB.Model(&model.User{}).Count(&users).Error)
	assert.EqualValues(t, 1, users, "root login must not create a user")

	// Emergency path: flip the option row with raw SQL, no restart, only a reload.
	require.NoError(t, model.DB.Exec(`INSERT INTO options ("key", value) VALUES (?, ?) ON CONFLICT ("key") DO UPDATE SET value = EXCLUDED.value`, "PasswordLoginEnabled", "true").Error)
	model.InitOptionMap()
	require.True(t, common.PasswordLoginEnabled)
	assert.False(t, common.PasswordRegisterEnabled, "only the named option changes")
	restored := passwordLogin()
	assert.Contains(t, restored.Body.String(), `"success":true`)
	assert.Contains(t, restored.Body.String(), `"access_token"`)
}
