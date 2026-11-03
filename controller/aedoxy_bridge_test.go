package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
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
