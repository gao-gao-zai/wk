package haozhuma

import "testing"

func TestSMSProfilesNormalizationDoesNotMutateSavedCredentials(t *testing.T) {
	s := Settings{Provider: " ecode ", Token: "wrong-legacy-projection", Sid: "52283", EcodeKeyword: "腾讯科技", Credentials: &PlatformCredentials{
		Haozhuma: Credentials{User: "hao-fixture", Pass: "pass-fixture"}, Ecode: Credentials{Token: "ec-fixture"},
	}}
	n := s.Normalized()
	if n.Provider != "ecode" || n.Token != "ec-fixture" || n.User != "" || n.Pass != "" {
		t.Fatal("incorrect selected platform projection")
	}
	n.Credentials.Haozhuma.User = "modified-fixture"
	if s.Credentials.Haozhuma.User != "hao-fixture" || s.Token != "wrong-legacy-projection" {
		t.Fatal("normalization mutated input")
	}
}
