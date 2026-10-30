// Copyright 2026 Srikanth K.
// SPDX-License-Identifier: Apache-2.0

package freebusy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// signedDelivery signs body under secret the way freebusy does.
func signedDelivery(t *testing.T, secret string, body []byte) http.Header {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(secret[len("whsec_"):])
	if err != nil {
		t.Fatal(err)
	}
	id, ts := "msg_1", strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	h := http.Header{}
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", ts)
	h.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return h
}

func TestVerifyWebhook_Secrets(t *testing.T) {
	good := "whsec_" + base64.StdEncoding.EncodeToString([]byte("the right key"))
	other := "whsec_" + base64.StdEncoding.EncodeToString([]byte("another key"))
	const invalid = "whsec_not*base64!"
	body := []byte(`{"type":"freebusy.hold.active"}`)
	header := signedDelivery(t, good, body)

	for _, c := range []struct {
		name    string
		secrets []string
		ok      bool
		badMsg  bool // the error names a bad secret
	}{
		{"the good secret", []string{good}, true, false},
		{"an invalid secret before the good one", []string{invalid, good}, true, false},
		{"an invalid secret after the good one", []string{good, invalid}, true, false},
		{"a wrong secret only", []string{other}, false, false},
		{"an invalid and a wrong secret", []string{invalid, other}, false, true},
		{"no secrets", nil, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := VerifyWebhook(header, body, c.secrets...)
			if c.ok {
				if err != nil {
					t.Fatalf("VerifyWebhook: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrWebhookSignature) {
				t.Fatalf("VerifyWebhook = %v, want ErrWebhookSignature", err)
			}
			if got := err.Error() != ErrWebhookSignature.Error(); got != c.badMsg {
				t.Errorf("error %q: names a bad secret = %v, want %v", err, got, c.badMsg)
			}
		})
	}
}
