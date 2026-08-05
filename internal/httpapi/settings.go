// 管理端设置 API (Task: 管理后台设置栏).
//
// 路由 (均受 RequireAdmin 保护):
//
//	GET  /api/admin/settings            -> 当前设置 (密码是否已配置 + 评分方式, API key 打码)
//	POST /api/admin/settings/password   {old_password, new_password}  改管理密码 (成功后旧 token 全废)
//	POST /api/admin/settings/scoring    {method, rerank_api_url, rerank_api_key, rerank_model,
//	                                      llm_api_url, llm_api_key, llm_model}  存评分方式
//
// 密码校验优先级: DB bcrypt hash (app_settings) > config.yaml admin.password 明文.
// 首个密码写入 DB 后, 后续校验一律走 DB hash (config 明文仅作首改的旧密码来源).
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/yhwyxy/examSystem/internal/auth"
	"github.com/yhwyxy/examSystem/internal/settings"
)

// maskKey 把凭据打码: 仅展示首尾 2 字符, 中间以 **** 掩蔽; 空值原样返回.
func maskKey(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return string(r[:2]) + "****" + string(r[len(r)-2:])
}

// setConfigured 返回某字段是否已配置 (不泄露内容).
func setConfigured(s string) bool { return s != "" }

// settingsScoringView 是 GET 返回给前端的评分方式视图 (API key 打码).
type settingsScoringView struct {
	Method          string `json:"method"`
	RerankAPIURL    string `json:"rerank_api_url"`
	RerankAPIKey    string `json:"rerank_api_key_masked"`
	RerankAPIKeySet bool   `json:"rerank_api_key_set"`
	RerankModel     string `json:"rerank_model"`
	LLMAPIURL       string `json:"llm_api_url"`
	LLMAPIPKey      string `json:"llm_api_key_masked"`
	LLMAPIPKeySet   bool   `json:"llm_api_key_set"`
	LLMModel        string `json:"llm_model"`
}

func scoringView(cfg *settings.ScoringConfig) settingsScoringView {
	return settingsScoringView{
		Method:          string(cfg.Method),
		RerankAPIURL:    cfg.RerankAPIURL,
		RerankAPIKey:    maskKey(cfg.RerankAPIKey),
		RerankAPIKeySet: setConfigured(cfg.RerankAPIKey),
		RerankModel:     cfg.RerankModel,
		LLMAPIURL:       cfg.LLMAPIURL,
		LLMAPIPKey:      maskKey(cfg.LLMAPIPKey),
		LLMAPIPKeySet:   setConfigured(cfg.LLMAPIPKey),
		LLMModel:        cfg.LLMModel,
	}
}

// getSettingsHandler GET /api/admin/settings.
func getSettingsHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Settings == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "SETTINGS_NOT_CONFIGURED",
				"settings store missing")
			return
		}
		ctx := r.Context()
		// 密码是否已配置: DB bcrypt hash 存在, 或 config 明文可用.
		passwordConfigured := false
		if deps.Config != nil && deps.Config.Admin.Password != nil &&
			*deps.Config.Admin.Password != "" {
			passwordConfigured = true
		}
		doc, err := deps.Settings.LoadPasswordHash(ctx)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "SETTINGS_LOAD_FAILED", err.Error())
			return
		}
		if doc != nil {
			passwordConfigured = true
		}
		// 评分方式: DB 优先, 回退 env 默认.
		scoring, err := deps.Settings.LoadScoring(ctx)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "SETTINGS_LOAD_FAILED", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"password_configured": passwordConfigured,
			"scoring":             scoringView(scoring),
		})
	}
}

// changePasswordHandler POST /api/admin/settings/password.
func changePasswordHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Settings == nil || deps.Auth == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "SETTINGS_NOT_CONFIGURED",
				"settings/auth store missing")
			return
		}
		var body struct {
			OldPassword string `json:"old_password"`
			NewPassword string `json:"new_password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		if body.NewPassword == "" || body.OldPassword == "" {
			writeAdminError(w, http.StatusBadRequest, "INVALID_REQUEST",
				"old_password and new_password required")
			return
		}
		err := deps.Auth.ChangePassword(r.Context(), body.OldPassword, body.NewPassword)
		if err != nil {
			status, code, msg := mapSettingsPasswordErr(err)
			writeAdminError(w, status, code, msg)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"message": "密码已更新, 请使用新密码重新登录",
		})
	}
}

func mapSettingsPasswordErr(err error) (int, string, string) {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return http.StatusUnauthorized, "OLD_PASSWORD_INCORRECT", "旧密码错误"
	case errors.Is(err, auth.ErrWeakPassword):
		return http.StatusBadRequest, "WEAK_PASSWORD", "新密码过短或与旧密码相同 (至少 6 位)"
	case errors.Is(err, auth.ErrPasswordStoreNotConfigured):
		return http.StatusServiceUnavailable, "PASSWORD_STORE_NOT_CONFIGURED",
			"未配置密码存储 (settings 未接线)"
	case errors.Is(err, auth.ErrAuthDisabled):
		return http.StatusServiceUnavailable, "AUTH_DISABLED", "管理认证未启用"
	case errors.Is(err, settings.ErrWeakPassword):
		return http.StatusBadRequest, "WEAK_PASSWORD", "新密码过短 (至少 6 位)"
	}
	return http.StatusInternalServerError, "PASSWORD_CHANGE_FAILED", err.Error()
}

// saveScoringHandler POST /api/admin/settings/scoring.
func saveScoringHandler(deps Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Settings == nil {
			writeAdminError(w, http.StatusServiceUnavailable, "SETTINGS_NOT_CONFIGURED",
				"settings store missing")
			return
		}
		var body struct {
			Method       string `json:"method"`
			RerankAPIURL string `json:"rerank_api_url"`
			RerankAPIKey string `json:"rerank_api_key"`
			RerankModel  string `json:"rerank_model"`
			LLMAPIURL    string `json:"llm_api_url"`
			LLMAPIPKey   string `json:"llm_api_key"`
			LLMModel     string `json:"llm_model"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_JSON", err.Error())
			return
		}
		method, err := settings.ParseScoringMethod(body.Method)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_METHOD", err.Error())
			return
		}
		cfg := &settings.ScoringConfig{
			Method:       method,
			RerankAPIURL: body.RerankAPIURL,
			RerankAPIKey: body.RerankAPIKey,
			RerankModel:  body.RerankModel,
			LLMAPIURL:    body.LLMAPIURL,
			LLMAPIPKey:   body.LLMAPIPKey,
			LLMModel:     body.LLMModel,
		}
		if err := cfg.Validate(); err != nil {
			writeAdminError(w, http.StatusBadRequest, "INVALID_SCORING_CONFIG", err.Error())
			return
		}
		if err := deps.Settings.SaveScoring(r.Context(), cfg); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "SETTINGS_SAVE_FAILED", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"scoring": scoringView(cfg),
		})
	}
}
