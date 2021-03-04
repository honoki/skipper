package auth_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/zalando/skipper/eskip"
	"github.com/zalando/skipper/filters/auth"
	"github.com/zalando/skipper/filters/builtin"
	"github.com/zalando/skipper/proxy/proxytest"
	"github.com/zalando/skipper/routing"
	"github.com/zalando/skipper/secrets"
)

const (
	testToken                = "test-token"
	testRefreshToken         = "refreshfoobarbaz"
	testAccessTokenExpiresIn = time.Hour
	testClientID             = "some-id"
	testClientSecret         = "some-secret"
	testAccessCode           = "quxquuxquz"
	testSecretFile           = "testdata/authsecret"
	testCookieName           = "testcookie"
	testQueryParamKey        = "param_key"
	testQueryParamValue      = "param_value"
)

func newGrantTestTokeninfo(validToken string, tokenInfoJSON string) *httptest.Server {
	const prefix = "Bearer "

	if tokenInfoJSON == "" {
		tokenInfoJSON = "{}"
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := func(code int) {
			w.WriteHeader(code)
			w.Write([]byte(tokenInfoJSON))
		}

		token := r.Header.Get("Authorization")
		if !strings.HasPrefix(token, prefix) || token[len(prefix):] != validToken {
			response(http.StatusUnauthorized)
			return
		}

		response(http.StatusOK)
	}))
}

func newGrantTestAuthServer(testToken, testAccessCode string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := func(w http.ResponseWriter, r *http.Request) {
			rq := r.URL.Query()
			redirect := rq.Get("redirect_uri")
			log.Debugf("redirect_uri: %v", redirect)
			rd, err := url.Parse(redirect)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			q := rd.Query()
			q.Set("code", testAccessCode)
			q.Set("state", r.URL.Query().Get("state"))
			rd.RawQuery = q.Encode()

			http.Redirect(
				w,
				r,
				rd.String(),
				http.StatusTemporaryRedirect,
			)
		}

		token := func(w http.ResponseWriter, r *http.Request) {
			var code, refreshToken string

			grantType := r.FormValue("grant_type")

			switch grantType {
			case "authorization_code":
				code = r.FormValue("code")
				if code != testAccessCode {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
			case "refresh_token":
				refreshToken = r.FormValue("refresh_token")
				if refreshToken != testRefreshToken {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
			}

			type tokenJSON struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				ExpiresIn    int    `json:"expires_in"`
			}

			token := tokenJSON{
				AccessToken:  testToken,
				RefreshToken: testRefreshToken,
				ExpiresIn:    int(testAccessTokenExpiresIn / time.Second),
			}

			b, err := json.Marshal(token)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		}

		switch r.URL.Path {
		case "/auth":
			auth(w, r)
		case "/token":
			token(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newGrantTestClusterAuthServer(authURL string) *httptest.Server {
	defaultRedirectURI := "/.well-known/oauth2-callback"
	var clientRedirectURI string
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHandler := func(w http.ResponseWriter, r *http.Request) {
			rq := r.URL.Query()
			// validated client redirect_uri ahead of time
			// TODO: save client redirect_uri by client_id
			log.Debugf("(/auth) client_id: %v", rq.Get("client_id"))
			clientRedirectURI = rq.Get("redirect_uri")
			_, err := url.Parse(clientRedirectURI)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			rd, err := url.Parse(authURL + "/auth")
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			q := rd.Query()
			rdURI := "http://" + r.Host + defaultRedirectURI
			log.Debugf("setting redirect_uri: %v", rdURI)
			q.Set("redirect_uri", rdURI)
			rd.RawQuery = q.Encode()
			http.Redirect(
				w,
				r,
				rd.String(),
				http.StatusTemporaryRedirect,
			)
		}

		callback := func(w http.ResponseWriter, r *http.Request) {
			log.Debugf("(/callback) client_id: %v", r.URL.Query().Get("client_id"))
			http.Redirect(
				w,
				r,
				clientRedirectURI,
				http.StatusTemporaryRedirect,
			)
		}

		switch r.URL.Path {
		case "/auth":
			authHandler(w, r)
		case defaultRedirectURI:
			callback(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newGrantTestConfig(tokeninfoURL, providerURL string) *auth.OAuthConfig {
	return &auth.OAuthConfig{
		ClientID:          testClientID,
		ClientSecret:      testClientSecret,
		Secrets:           secrets.NewRegistry(),
		SecretFile:        testSecretFile,
		TokeninfoURL:      tokeninfoURL,
		AuthURL:           providerURL + "/auth",
		TokenURL:          providerURL + "/token",
		RevokeTokenURL:    providerURL + "/revoke",
		TokenCookieName:   testCookieName,
		AuthURLParameters: map[string]string{testQueryParamKey: testQueryParamValue},
	}
}

func newAuthProxy(config *auth.OAuthConfig, routes ...*eskip.Route) (*proxytest.TestProxy, error) {
	err := config.Init()
	if err != nil {
		return nil, err
	}

	grantSpec := config.NewGrant()

	grantCallbackSpec := config.NewGrantCallback()

	grantClaimsQuerySpec := config.NewGrantClaimsQuery()

	grantPrep := config.NewGrantPreprocessor()

	grantLogoutSpec := config.NewGrantLogout()

	fr := builtin.MakeRegistry()
	fr.Register(grantSpec)
	fr.Register(grantCallbackSpec)
	fr.Register(grantClaimsQuerySpec)
	fr.Register(grantLogoutSpec)

	ro := routing.Options{
		PreProcessors: []routing.PreProcessor{grantPrep},
	}

	return proxytest.WithRoutingOptions(fr, ro, routes...), nil
}

func newSimpleGrantAuthProxy(t *testing.T, config *auth.OAuthConfig) *proxytest.TestProxy {
	proxy, err := newAuthProxy(config, &eskip.Route{
		Filters: []*eskip.Filter{
			{Name: auth.OAuthGrantName},
			{Name: "status", Args: []interface{}{http.StatusNoContent}},
		},
		BackendType: eskip.ShuntBackend,
	})

	if err != nil {
		t.Fatal(err)
	}

	return proxy
}

func newGrantHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newGrantCookie(config auth.OAuthConfig) (*http.Cookie, error) {
	return auth.NewGrantCookieWithExpiration(config, time.Now().Add(testAccessTokenExpiresIn))
}

func checkStatus(t *testing.T, rsp *http.Response, expectedStatus int) {
	if rsp.StatusCode != expectedStatus {
		t.Fatalf(
			"Unexpected status code, got: %d, expected: %d.",
			rsp.StatusCode,
			expectedStatus,
		)
	}
}

func checkRedirect(t *testing.T, rsp *http.Response, expectedURL string) {
	checkStatus(t, rsp, http.StatusTemporaryRedirect)
	redirectTo := rsp.Header.Get("Location")
	if !strings.HasPrefix(redirectTo, expectedURL) {
		t.Fatalf(
			"Unexpected redirect location, got: '%s', expected: '%s'.",
			redirectTo,
			expectedURL,
		)
	}
}

func findAuthCookie(rsp *http.Response) (*http.Cookie, bool) {
	if rsp == nil {
		return nil, false
	}

	for _, c := range rsp.Cookies() {
		if c.Name == testCookieName {
			return c, true
		}
	}

	return nil, false
}

func checkCookie(t *testing.T, rsp *http.Response) {
	if rsp == nil {
		t.Fatalf("Response is nill")
	}

	c, ok := findAuthCookie(rsp)
	if !ok {
		t.Fatalf("Cookie not found.")
	}

	if c.Value == "" {
		t.Fatalf("Cookie deleted.")
	}

	if !c.Secure {
		t.Fatalf("Cookie not secure")
	}

	if !c.HttpOnly {
		t.Fatalf("Cookie not HTTP only")
	}

	accessTokenExpiryTime := time.Now().Add(testAccessTokenExpiresIn)
	if c.Expires.Before(accessTokenExpiryTime) || c.Expires == accessTokenExpiryTime {
		t.Fatalf("Cookie expires with or before access token.")
	}
}

func grantQueryWithCookie(t *testing.T, client *http.Client, url string, cookies ...*http.Cookie) *http.Response {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}

	rsp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer rsp.Body.Close()

	return rsp
}

func TestGrantFlow(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	provider := newGrantTestAuthServer(testToken, testAccessCode)
	defer provider.Close()
	log.Infof("provider URL: %s", provider.URL)

	clusterAuth := newGrantTestClusterAuthServer(provider.URL)
	defer clusterAuth.Close()
	log.Infof("cluster URL: %s", clusterAuth.URL)

	tokeninfo := newGrantTestTokeninfo(testToken, "")
	defer tokeninfo.Close()

	config := newGrantTestConfig(tokeninfo.URL, clusterAuth.URL)

	proxy := newSimpleGrantAuthProxy(t, config)
	defer proxy.Close()
	log.Infof("proxy URL: %s", proxy.URL)

	client := newGrantHTTPClient()

	t.Run("check full grant flow", func(t *testing.T) {
		rsp, err := client.Get(proxy.URL)
		if err != nil {
			t.Fatal(err)
		}

		defer rsp.Body.Close()

		log.Debugf("1")
		checkRedirect(t, rsp, clusterAuth.URL+"/auth")

		rsp, err = client.Get(rsp.Header.Get("Location"))
		if err != nil {
			t.Fatalf("Failed to make request to cluster auth: %v.", err)
		}

		defer rsp.Body.Close()

		log.Debugf("2")
		checkRedirect(t, rsp, provider.URL+"/auth")

		rsp, err = client.Get(rsp.Header.Get("Location"))
		if err != nil {
			t.Fatalf("Failed to make request to provider: %v.", err)
		}

		defer rsp.Body.Close()

		log.Debugf("3")
		checkRedirect(t, rsp, clusterAuth.URL+"/.well-known/oauth2-callback")

		rsp, err = client.Get(rsp.Header.Get("Location"))
		if err != nil {
			t.Fatalf("Failed to make request to cluster auth: %v.", err)
		}

		defer rsp.Body.Close()

		log.Debugf("4")
		checkRedirect(t, rsp, proxy.URL+"/.well-known/oauth2-callback")

		rsp, err = client.Get(rsp.Header.Get("Location"))
		if err != nil {
			t.Fatalf("Failed to make request to proxy: %v.", err)
		}

		log.Debugf("5")
		checkRedirect(t, rsp, proxy.URL)
		checkCookie(t, rsp)

		req, err := http.NewRequest("GET", rsp.Header.Get("Location"), nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v.", err)
		}

		c, _ := findAuthCookie(rsp)
		req.Header.Set("Cookie", fmt.Sprintf("%s=%s", c.Name, c.Value))
		rsp, err = client.Do(req)
		if err != nil {
			t.Fatalf("Failed to make request to proxy: %v.", err)
		}

		checkStatus(t, rsp, http.StatusNoContent)
	})

	t.Run("check login is triggered access token is invalid", func(t *testing.T) {
		cookie, err := auth.NewGrantCookieWithInvalidAccessToken(*config)
		if err != nil {
			t.Fatal(err)
		}

		rsp := grantQueryWithCookie(t, client, proxy.URL, cookie)

		checkRedirect(t, rsp, provider.URL+"/auth")
	})

	t.Run("check login is triggered when cookie is corrupted", func(t *testing.T) {
		url, _ := url.Parse(proxy.URL)
		cookie := &http.Cookie{
			Name:     config.TokenCookieName,
			Value:    "corruptedcookievalue",
			Path:     "/",
			Domain:   url.Hostname(),
			Expires:  time.Now().Add(time.Duration(1) * time.Hour),
			Secure:   true,
			HttpOnly: true,
		}

		rsp := grantQueryWithCookie(t, client, proxy.URL, cookie)

		checkRedirect(t, rsp, provider.URL+"/auth")
	})

	t.Run("check handles multiple cookies with same name and uses the first decodable one", func(t *testing.T) {
		badCookie, _ := newGrantCookie(*config)
		badCookie.Value = "invalid"
		goodCookie, _ := newGrantCookie(*config)

		rsp := grantQueryWithCookie(t, client, proxy.URL, badCookie, goodCookie)

		checkStatus(t, rsp, http.StatusNoContent)
	})

	t.Run("check does not send cookie again if token was not refreshed", func(t *testing.T) {
		goodCookie, _ := newGrantCookie(*config)

		rsp := grantQueryWithCookie(t, client, proxy.URL, goodCookie)

		_, ok := findAuthCookie(rsp)
		if ok {
			t.Fatalf(
				"The auth cookie should only be added to the response if there was a refresh.",
			)
		}
	})
}

func TestGrantRefresh(t *testing.T) {
	provider := newGrantTestAuthServer(testToken, testAccessCode)
	defer provider.Close()

	tokeninfo := newGrantTestTokeninfo(testToken, "")
	defer tokeninfo.Close()

	config := newGrantTestConfig(tokeninfo.URL, provider.URL)

	client := newGrantHTTPClient()

	proxy := newSimpleGrantAuthProxy(t, config)
	defer proxy.Close()

	t.Run("check token is refreshed if it expired", func(t *testing.T) {
		cookie, err := auth.NewGrantCookieWithExpiration(*config, time.Now().Add(time.Duration(-1)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}

		rsp := grantQueryWithCookie(t, client, proxy.URL, cookie)

		checkStatus(t, rsp, http.StatusNoContent)
	})

	t.Run("check login is triggered if refresh token is invalid", func(t *testing.T) {
		cookie, err := auth.NewGrantCookieWithInvalidRefreshToken(*config)
		if err != nil {
			t.Fatal(err)
		}

		rsp := grantQueryWithCookie(t, client, proxy.URL, cookie)

		checkRedirect(t, rsp, provider.URL+"/auth")
	})
}
