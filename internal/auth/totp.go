package auth

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"image/png"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/pquerna/otp/totp"
)

const totpPeriod = 30 // seconds

// TOTPSetup is a freshly generated authenticator secret, ready to show.
type TOTPSetup struct {
	Secret string // base32, for manual entry
	URL    string // otpauth:// URL
	QR     string // data: URI of a PNG QR code
}

func NewTOTP(issuer, account string) (TOTPSetup, error) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: issuer, AccountName: account, Period: totpPeriod})
	if err != nil {
		return TOTPSetup{}, err
	}
	return TOTPSetupFor(key.Secret(), issuer, account)
}

// TOTPSetupFor rebuilds the setup view for an existing secret.
func TOTPSetupFor(secret, issuer, account string) (TOTPSetup, error) {
	u := url.URL{
		Scheme: "otpauth", Host: "totp", Path: "/" + issuer + ":" + account,
		RawQuery: url.Values{
			"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"},
			"digits": {"6"}, "period": {strconv.Itoa(totpPeriod)},
		}.Encode(),
	}
	key, err := otp.NewKeyFromURL(u.String())
	if err != nil {
		return TOTPSetup{}, err
	}
	img, err := key.Image(220, 220)
	if err != nil {
		return TOTPSetup{}, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return TOTPSetup{}, err
	}
	return TOTPSetup{
		Secret: secret,
		URL:    key.URL(),
		QR:     "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

// VerifyTOTP checks a 6-digit code against secret, allowing one step of
// clock drift either way. A code is accepted only for a step later than
// lastStep, so each code works once; the matched step is returned so the
// caller can store it.
func VerifyTOTP(secret, code string, lastStep int64, now time.Time) (step int64, ok bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	for s := cur - 1; s <= cur+1; s++ {
		if s <= lastStep {
			continue
		}
		want, err := hotp.GenerateCodeCustom(secret, uint64(s), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}
