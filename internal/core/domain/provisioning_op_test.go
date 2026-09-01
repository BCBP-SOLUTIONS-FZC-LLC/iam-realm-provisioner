package domain

import "testing"

func TestIsAllowedSettingKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"exact allow-listed key", "local_accounts_enabled", true},
		{"idp prefix with alias", "idp:google", true},
		{"idp prefix with single char alias", "idp:x", true},
		{"idp prefix with no alias is too short", "idp:", false},
		{"unrelated key", "some_other_setting", false},
		{"empty key", "", false},
		{"case-sensitive prefix mismatch", "IDP:google", false},
		{"prefix substring without colon", "idpgoogle", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAllowedSettingKey(tc.key); got != tc.want {
				t.Errorf("IsAllowedSettingKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}
