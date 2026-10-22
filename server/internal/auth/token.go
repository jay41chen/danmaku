package auth

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	signingMethod   = jwt.SigningMethodHS256
)

// access token（JWT，HS256）的簽發與驗證
type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
}

func NewTokenIssuer(secret string, ttl time.Duration) (*TokenIssuer, error) {
	if err := ValidateJWTSecretLength(secret); err != nil {
		return nil, err
	}
	issuer := &TokenIssuer{secret: []byte(secret), ttl: ttl}
	return issuer, nil
}

// 一起回傳效期：「簽進 exp 的」跟「告訴 client 的 expires_in」保證是同一次呼叫的結果
func (this *TokenIssuer) IssueAccessToken(userID int64) (token string, expiresIn time.Duration, err error) {
	sub := strconv.FormatInt(userID, 10)
	now := time.Now()
	iat := jwt.NewNumericDate(now)
	exp := jwt.NewNumericDate(now.Add(this.ttl))
	// 包裝進jwt
	claims := jwt.RegisteredClaims{
		Subject:   sub,
		IssuedAt:  iat,
		ExpiresAt: exp,
	}
	//用 HS256 + secret 簽成字串
	token, err = jwt.NewWithClaims(signingMethod, claims).SignedString(this.secret)
	if err != nil {
		return "", 0, err
	}
	return token, this.ttl, nil
}

func (this *TokenIssuer) ParseAccessToken(token string) (int64, error) {
	var claims jwt.RegisteredClaims
	// 傳address進去，套件解析後會把 payload 填進 `claims`
	_, err := jwt.ParseWithClaims(token, &claims,
		func(t *jwt.Token) (any, error) {
			// keyFunc：要用哪把 key 驗證
			return this.secret, nil
		},
		jwt.WithValidMethods([]string{signingMethod.Alg()}), // 演算法的「名稱字串」
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	// 檢查塞進來的id是否合法
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	return id, nil
}
