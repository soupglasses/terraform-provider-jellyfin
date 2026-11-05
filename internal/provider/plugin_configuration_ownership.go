// Copyright Sofie Finnes 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	"github.com/ThePhaseless/terraform-provider-jellyfin/internal/client"
)

func configurationObject(raw string) (map[string]any, error) {
	normalized, err := normalizeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("configuration must be a valid JSON object")
	}
	value, err := decodeJSON(normalized)
	if err != nil {
		return nil, fmt.Errorf("configuration must be a valid JSON object")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("configuration must be a JSON object")
	}
	return object, nil
}

// Record keys only, so ownership metadata never duplicates configured secrets.
func configurationOwnership(raw string) ([]byte, error) {
	object, err := configurationObject(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(ownershipShape(object))
}

func ownershipShape(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	shape := make(map[string]any, len(object))
	for key, child := range object {
		shape[key] = ownershipShape(child)
	}
	return shape
}

func mergeConfiguration(current string, desired map[string]any) (string, error) {
	object, err := configurationObject(current)
	if err != nil {
		return "", fmt.Errorf("server configuration must be a JSON object")
	}
	merged, err := json.Marshal(overlayConfiguration(object, desired))
	if err != nil {
		return "", err
	}
	return string(merged), nil
}

func overlayConfiguration(current, desired map[string]any) map[string]any {
	for key, value := range desired {
		if object, ok := value.(map[string]any); ok {
			existing, ok := current[key].(map[string]any)
			if !ok {
				existing = make(map[string]any)
			}
			current[key] = overlayConfiguration(existing, object)
		} else {
			current[key] = value
		}
	}
	return current
}

// servedConfiguration returns only owned object keys. Arrays are owned whole.
func servedConfiguration(served string, managed map[string]any) (string, error) {
	servedValue, err := configurationObject(served)
	if err != nil {
		return "", fmt.Errorf("parsing the served configuration: %w", err)
	}
	kept, err := json.Marshal(keysOf(servedValue, managed))
	if err != nil {
		return "", err
	}
	return string(kept), nil
}

func decodeJSON(raw string) (any, error) {
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// keysOf returns served with only the object keys shape has, at any depth,
// treating arrays as atomic values.
func keysOf(served, shape any) any {
	switch s := shape.(type) {
	case map[string]any:
		m, ok := served.(map[string]any)
		if !ok {
			return served
		}
		out := make(map[string]any, len(s))
		for k, v := range s {
			if sv, ok := m[k]; ok {
				out[k] = keysOf(sv, v)
			}
		}
		return out
	}
	return served
}

type configurationObjectValidator struct{}

func (configurationObjectValidator) Description(context.Context) string {
	return "Configuration must be a JSON object."
}

func (v configurationObjectValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (configurationObjectValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := configurationObject(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid plugin configuration", err.Error())
	}
}

// A plugin error response can echo credentials from the submitted document.
func configurationError(err error) string {
	var response *client.HTTPError
	if errors.As(err, &response) {
		return fmt.Sprintf("%s %s returned status %d", response.Method, response.Path, response.StatusCode)
	}
	return err.Error()
}
