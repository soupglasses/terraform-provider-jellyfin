// Copyright Sofie Finnes 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"
)

func TestUnitPluginConfigurationMerge(t *testing.T) {
	cases := []struct{ name, current, desired, want string }{
		{"runtime mappings", `{"LdapUsers":[{"id":"123"}],"Host":"old"}`, `{"Host":"new"}`, `{"Host":"new","LdapUsers":[{"id":"123"}]}`},
		{"nested providers", `{"OIDC":{"a":{"secret":"keep","enabled":false},"b":{"enabled":true}}}`, `{"OIDC":{"a":{"enabled":true}}}`, `{"OIDC":{"a":{"enabled":true,"secret":"keep"},"b":{"enabled":true}}}`},
		{"atomic array", `{"users":[{"id":1,"extra":true},{"id":2}]}`, `{"users":[{"id":3}]}`, `{"users":[{"id":3}]}`},
		{"explicit null", `{"value":{"other":1},"keep":2}`, `{"value":null}`, `{"keep":2,"value":null}`},
		{"empty root", `{"keep":9007199254740993}`, `{}`, `{"keep":9007199254740993}`},
		{"empty nested", `{"value":{"keep":1}}`, `{"value":{}}`, `{"value":{"keep":1}}`},
		{"replace scalar with object", `{"value":false}`, `{"value":{}}`, `{"value":{}}`},
		{"replace object with scalar", `{"value":{"keep":1}}`, `{"value":true}`, `{"value":true}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			desired, err := configurationObject(c.desired)
			if err != nil {
				t.Fatal(err)
			}
			got, err := mergeConfiguration(c.current, desired)
			if err != nil {
				t.Fatal(err)
			}
			want, err := normalizeJSON(c.want)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("got %s; want %s", got, want)
			}
		})
	}
	for _, invalid := range []string{`null`, `[]`, `1`, `true`, `"secret"`, `{} {}`} {
		if _, err := configurationObject(invalid); err == nil {
			t.Errorf("accepted %s", invalid)
		}
		if _, err := mergeConfiguration(invalid, map[string]any{}); err == nil {
			t.Errorf("accepted server %s", invalid)
		}
	}
}

func TestUnitPluginConfigurationOwnershipContainsOnlyKeys(t *testing.T) {
	mask, err := configurationOwnership(`{"section":{"secret":"never store this","array":[{"value":1}]},"empty":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	if string(mask) != `{"empty":{},"section":{"array":null,"secret":null}}` {
		t.Fatalf("unexpected ownership %s", mask)
	}
}
