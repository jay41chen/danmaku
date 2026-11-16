package model

import "time"

// login / refresh 發出的一組 token；refresh token 是明文，只在回應裡出現一次，不能進 log
type TokenPair struct {
	AccessToken  string
	ExpiresIn    time.Duration
	RefreshToken string
}
