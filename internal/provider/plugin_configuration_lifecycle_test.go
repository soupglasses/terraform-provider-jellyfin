// Copyright Sofie Finnes 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Exercise Terraform's plan/apply/refresh contract against a replacement-only
// API, including states that a typed plugin may not normally return.
func TestUnitPluginConfigurationLifecycle(t *testing.T) {
	var mu sync.Mutex
	stored := `{"section":{"owned":1,"runtime":"keep"}}`
	posts := 0
	set := func(raw string) {
		mu.Lock()
		defer mu.Unlock()
		stored = raw
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/System/Info/Public" {
			_, _ = fmt.Fprintf(w, `{"StartupWizardCompleted":true,"Version":%q}`, supportedJellyfinVersion())
			return
		}
		if r.URL.Path != "/Plugins/8c95c4d2e50c4fb0a4f36c06ff0f9a1a/Configuration" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "cannot read", http.StatusBadRequest)
				return
			}
			stored = string(body)
			posts++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, stored)
	}))
	t.Cleanup(server.Close)
	providerConfig := fmt.Sprintf(`provider "jellyfin" {
  endpoint = %q
  api_key  = "test"
}
`, server.URL)
	config := func(raw string) string {
		return providerConfig + fmt.Sprintf(`resource "jellyfin_plugin_configuration" "test" {
  plugin_id          = "8c95c4d2e50c4fb0a4f36c06ff0f9a1a"
  configuration_json = %q
}`, raw)
	}
	check := func(want string, writes int) resource.TestCheckFunc {
		return func(_ *terraform.State) error {
			mu.Lock()
			defer mu.Unlock()
			normalized, err := normalizeJSON(want)
			if err != nil {
				return err
			}
			actual, err := normalizeJSON(stored)
			if err != nil {
				return err
			}
			if actual != normalized || posts != writes {
				return fmt.Errorf("remote = %s, writes = %d; want %s, %d", stored, posts, want, writes)
			}
			return nil
		}
	}
	emptyPlan := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}
	desired := config(`{"section":{"owned":2}}`)
	unknownConfig := providerConfig + `
resource "terraform_data" "settings" {
  input = jsonencode({ section = {} })
}
resource "jellyfin_plugin_configuration" "test" {
  plugin_id          = "8c95c4d2e50c4fb0a4f36c06ff0f9a1a"
  configuration_json = terraform_data.settings.output
}`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: desired,
				Check:  check(`{"section":{"owned":2,"runtime":"keep"}}`, 1),
			},
			{
				Config:           desired,
				ConfigPlanChecks: emptyPlan,
			},
			{
				PreConfig:          func() { set(`{}`) },
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
			{
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig:        func() { set(`{"section":{"owned":2,"runtime":"new"}}`) },
				Config:           desired,
				ConfigPlanChecks: emptyPlan,
			},
			{
				PreConfig:          func() { set(`{"section":false}`) },
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
			{
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: desired,
			},
			{
				PreConfig: func() { set(`{}`) },
				Config:    config(`{}`),
			},
			{
				PreConfig:        func() { set(`{"section":{"owned":99}}`) },
				Config:           config(`{}`),
				ConfigPlanChecks: emptyPlan,
			},
			// Adopting existing settings and narrowing an import must not write.
			{
				Config: providerConfig,
				Check:  check(`{"section":{"owned":99}}`, 2),
			},
			{
				Config:             config(`{}`),
				ResourceName:       "jellyfin_plugin_configuration.test",
				ImportState:        true,
				ImportStateId:      "8c95c4d2e50c4fb0a4f36c06ff0f9a1a",
				ImportStatePersist: true,
			},
			{
				Config: config(`{}`),
				Check:  check(`{"section":{"owned":99}}`, 2),
			},
			// A missing explicit null is drift; omission releases it even if
			// the server already removed the key before the configuration did.
			{
				Config: config(`{"nullable":null}`),
				Check:  check(`{"nullable":null,"section":{"owned":99}}`, 3),
			},
			{
				PreConfig: func() { set(`{"section":{"owned":99}}`) },
				Config:    config(`{"nullable":null}`),
				Check:     check(`{"nullable":null,"section":{"owned":99}}`, 4),
			},
			{
				PreConfig: func() { set(`{}`) },
				Config:    config(`{}`),
				Check:     check(`{}`, 4),
			},
			{
				PreConfig:        func() { set(`{"nullable":"external"}`) },
				Config:           config(`{}`),
				ConfigPlanChecks: emptyPlan,
			},
			// Arrays are atomic, including object elements, and an empty
			// object preserves children while requiring the object to exist.
			{
				Config: config(`{"list":[{"id":1}],"section":{}}`),
				Check:  check(`{"list":[{"id":1}],"nullable":"external","section":{}}`, 5),
			},
			{
				PreConfig: func() { set(`{"list":[{"id":1,"extra":true}],"nullable":"external","section":{"keep":true}}`) },
				Config:    config(`{"list":[{"id":1}],"section":{}}`),
				Check:     check(`{"list":[{"id":1}],"nullable":"external","section":{"keep":true}}`, 6),
			},
			{
				Config: config(`{"list":[],"section":{}}`),
				Check:  check(`{"list":[],"nullable":"external","section":{"keep":true}}`, 7),
			},
			{
				Config: config(`{"section":{}}`),
				Check:  check(`{"list":[],"nullable":"external","section":{"keep":true}}`, 7),
			},
			{
				PreConfig:        func() { set(`{"list":[2],"section":{"new":true}}`) },
				Config:           config(`{"section":{}}`),
				ConfigPlanChecks: emptyPlan,
			},
			{
				Config: unknownConfig,
				Check:  check(`{"list":[2],"section":{"new":true}}`, 7),
			},
			{
				Config:           unknownConfig,
				ConfigPlanChecks: emptyPlan,
			},
		},
	})
	mu.Lock()
	defer mu.Unlock()
	if posts != 7 || !strings.Contains(stored, `"new":true`) {
		t.Errorf("destroy changed the configuration: %s (%d writes)", stored, posts)
	}
}
