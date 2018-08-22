// Copyright (c) 2017 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/astaxie/beego"
	"github.com/beego/i18n"
	jwtgo "github.com/dgrijalva/jwt-go"
	"github.com/vmware/harbor/src/common"
	"github.com/vmware/harbor/src/common/dao"
	"github.com/vmware/harbor/src/common/models"
	"github.com/vmware/harbor/src/common/utils"
	email_util "github.com/vmware/harbor/src/common/utils/email"
	"github.com/vmware/harbor/src/common/utils/log"
	"github.com/vmware/harbor/src/ui/auth"
	"github.com/vmware/harbor/src/ui/config"
	"golang.org/x/oauth2"
)

// CommonController handles request from UI that doesn't expect a page, such as /SwitchLanguage /logout ...
type CommonController struct {
	beego.Controller
	i18n.Locale
}

// Render returns nil.
func (cc *CommonController) Render() error {
	return nil
}

type messageDetail struct {
	Hint string
	URL  string
	UUID string
}

// Login handles login request from UI.
func (cc *CommonController) Login() {
	principal := cc.GetString("principal")
	password := cc.GetString("password")

	user, err := auth.Login(models.AuthModel{
		Principal: principal,
		Password:  password,
	})
	if err != nil {
		log.Errorf("Error occurred in UserLogin: %v", err)
		cc.CustomAbort(http.StatusUnauthorized, "")
	}

	if user == nil {
		cc.CustomAbort(http.StatusUnauthorized, "")
	}

	cc.SetSession("userId", user.UserID)
	cc.SetSession("username", user.Username)
	cc.SetSession("isSysAdmin", user.HasAdminRole == 1)
}

// LogOut Habor UI
func (cc *CommonController) LogOut() {
	cc.DestroySession()
}

// UserExists checks if user exists when user input value in sign in form.
func (cc *CommonController) UserExists() {
	target := cc.GetString("target")
	value := cc.GetString("value")

	user := models.User{}
	switch target {
	case "username":
		user.Username = value
	case "email":
		user.Email = value
	}

	exist, err := dao.UserExists(user, target)
	if err != nil {
		log.Errorf("Error occurred in UserExists: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "Internal error.")
	}
	cc.Data["json"] = exist
	cc.ServeJSON()
}

// SendResetEmail verifies the Email address and contact SMTP server to send reset password Email.
func (cc *CommonController) SendResetEmail() {

	email := cc.GetString("email")

	valid, err := regexp.MatchString(`^(([^<>()[\]\\.,;:\s@\"]+(\.[^<>()[\]\\.,;:\s@\"]+)*)|(\".+\"))@((\[[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\])|(([a-zA-Z\-0-9]+\.)+[a-zA-Z]{2,}))$`, email)
	if err != nil {
		log.Errorf("failed to match regexp: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "Internal error.")
	}

	if !valid {
		cc.CustomAbort(http.StatusBadRequest, "invalid email")
	}

	queryUser := models.User{Email: email}
	u, err := dao.GetUser(queryUser)
	if err != nil {
		log.Errorf("Error occurred in GetUser: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "Internal error.")
	}
	if u == nil {
		log.Debugf("email %s not found", email)
		cc.CustomAbort(http.StatusNotFound, "email_does_not_exist")
	}

	if !isUserResetable(u) {
		log.Errorf("Resetting password for user with ID: %d is not allowed", u.UserID)
		cc.CustomAbort(http.StatusForbidden, http.StatusText(http.StatusForbidden))
	}

	uuid := utils.GenerateRandomString()
	user := models.User{ResetUUID: uuid, Email: email}
	if err = dao.UpdateUserResetUUID(user); err != nil {
		log.Errorf("failed to update user reset UUID: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
	}

	messageTemplate, err := template.ParseFiles("views/reset-password-mail.tpl")
	if err != nil {
		log.Errorf("Parse email template file failed: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, err.Error())
	}

	message := new(bytes.Buffer)

	harborURL, err := config.ExtEndpoint()
	if err != nil {
		log.Errorf("failed to get domain name: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
	}

	err = messageTemplate.Execute(message, messageDetail{
		Hint: cc.Tr("reset_email_hint"),
		URL:  harborURL,
		UUID: uuid,
	})

	if err != nil {
		log.Errorf("Message template error: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "internal_error")
	}

	settings, err := config.Email()
	if err != nil {
		log.Errorf("failed to get email configurations: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "internal_error")
	}

	addr := net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))
	err = email_util.Send(addr,
		settings.Identity,
		settings.Username,
		settings.Password,
		60, settings.SSL,
		settings.Insecure,
		settings.From,
		[]string{email},
		"Reset Harbor user password",
		message.String())
	if err != nil {
		log.Errorf("Send email failed: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "send_email_failed")
	}
}

// ResetPassword handles request from the reset page and reset password
func (cc *CommonController) ResetPassword() {

	resetUUID := cc.GetString("reset_uuid")
	if resetUUID == "" {
		cc.CustomAbort(http.StatusBadRequest, "Reset uuid is blank.")
	}

	queryUser := models.User{ResetUUID: resetUUID}
	user, err := dao.GetUser(queryUser)

	if err != nil {
		log.Errorf("Error occurred in GetUser: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "Internal error.")
	}
	if user == nil {
		log.Error("User does not exist")
		cc.CustomAbort(http.StatusBadRequest, "User does not exist")
	}

	if !isUserResetable(user) {
		log.Errorf("Resetting password for user with ID: %d is not allowed", user.UserID)
		cc.CustomAbort(http.StatusForbidden, http.StatusText(http.StatusForbidden))
	}

	password := cc.GetString("password")

	if password != "" {
		user.Password = password
		err = dao.ResetUserPassword(*user)
		if err != nil {
			log.Errorf("Error occurred in ResetUserPassword: %v", err)
			cc.CustomAbort(http.StatusInternalServerError, "Internal error.")
		}
	} else {
		cc.CustomAbort(http.StatusBadRequest, "password_is_required")
	}
}

// Oauth exchanges OAuth authorization codes for an access token and
// authenticates (and possibly creates) the user described in the token.
func (cc *CommonController) Oauth() {
	// {"name":"Harbor Dev","description":"for Harbor OAuth development","id":"6d3cca7a-5f59-4664-a513-6cb7783d50b0","secret":"7e67a166a29087c8079916e7a4df1c87aa8ea187d547f8e266b7ac98b058ad0c","callback_url":"http://harbor.appfound.co/oauth","signing":{"algorithm":"HS256","key":"a92d1e15853ff92ad0dd772ee3f2a98564526f7d4e3e2892764dbc066b0e61cd"}}
	customTLSContext := context.TODO()
	pool, err := x509.SystemCertPool()
	if err != nil {
		log.Errorf("error retrieving system cert pool: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "SSL error")
		return
	}

	client := &http.Client{
		Timeout: time.Minute,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:            pool,
				InsecureSkipVerify: true,
			},
		},
	}

	customTLSContext = context.WithValue(customTLSContext, oauth2.HTTPClient, client)

	cfg, err := config.OAuthConf()
	if err != nil {
		log.Errorf("Error loading config: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "error loading config")
		return
	}

	oc := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  cfg.AuthURL,
			TokenURL: cfg.TokenURL,
		},
	}

	token, err := oc.Exchange(customTLSContext, cc.Input().Get("code"))
	if err != nil {
		log.Errorf("Error calling oauth Exchange: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "error retrieving oauth token")
		return
	}

	data, err := jwtgo.ParseWithClaims(token.AccessToken, &jwtgo.StandardClaims{}, func(t *jwtgo.Token) (interface{}, error) {
		if t.Method != jwtgo.SigningMethodHS256 {
			return nil, fmt.Errorf("only HS256 signing is supported")
		}

		return []byte("a92d1e15853ff92ad0dd772ee3f2a98564526f7d4e3e2892764dbc066b0e61cd"), nil
	})

	if err != nil {
		log.Errorf("error parsing JWT: %v", err)
		cc.CustomAbort(http.StatusInternalServerError, "error parsing oauth response")
		return
	}

	log.Debugf("claim data: %+v", data.Claims.(*jwtgo.StandardClaims))
	user, err := createUser(&models.User{
		Username: data.Claims.(*jwtgo.StandardClaims).Subject,
		Email:    fmt.Sprintf("%s@%s", data.Claims.(*jwtgo.StandardClaims).Subject, data.Claims.(*jwtgo.StandardClaims).Issuer),
	})
	if err != nil {
		log.Errorf("error creating user: %v", err)
		cc.Abort("500")
		return
	}

	cc.SetSession("userId", user.UserID)
	cc.SetSession("username", user.Username)
	cc.SetSession("isSysAdmin", user.HasAdminRole == 1)

	cc.Redirect("/harbor", http.StatusFound)
	return
}

func isUserResetable(u *models.User) bool {
	if u == nil {
		return false
	}
	mode, err := config.AuthMode()
	if err != nil {
		log.Errorf("Failed to get the auth mode, error: %v", err)
		return false
	}
	if mode == common.DBAuth {
		return true
	}
	return u.UserID == 1
}

func createUser(u *models.User) (*models.User, error) {
	err := dao.OnBoardUser(u)
	if err != nil {
		return nil, err
	}

	return u, nil
}

func init() {
	//conf/app.conf -> os.Getenv("config_path")
	configPath := os.Getenv("CONFIG_PATH")
	if len(configPath) != 0 {
		log.Infof("Config path: %s", configPath)
		if err := beego.LoadAppConfig("ini", configPath); err != nil {
			log.Errorf("failed to load app config: %v", err)
		}
	}

}
