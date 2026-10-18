// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ThePhaseless/terraform-provider-jellyfin/internal/client"
)

var (
	_ resource.Resource                = &PluginConfigurationResource{}
	_ resource.ResourceWithImportState = &PluginConfigurationResource{}
	_ resource.ResourceWithModifyPlan  = &PluginConfigurationResource{}
)

// NewPluginConfigurationResource creates a new plugin configuration resource.
func NewPluginConfigurationResource() resource.Resource {
	return &PluginConfigurationResource{}
}

// PluginConfigurationResource defines the resource implementation.
type PluginConfigurationResource struct {
	client *client.Client
}

// PluginConfigurationResourceModel describes the resource data model.
type PluginConfigurationResourceModel struct {
	ID                types.String         `tfsdk:"id"`
	PluginID          types.String         `tfsdk:"plugin_id"`
	Configuration     jsontypes.Normalized `tfsdk:"configuration_json"`
	ConfigurationKeys jsontypes.Normalized `tfsdk:"configuration_keys"`
}

func (r *PluginConfigurationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_plugin_configuration"
}

func (r *PluginConfigurationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	const description = "Manages selected plugin configuration fields in Jellyfin using a JSON object. " +
		"Object keys merge recursively with the current server configuration; omitted keys are preserved. " +
		"Arrays, scalars and explicit null replace their entire values. Null is a value, not a deletion. " +
		"Empty objects require an object but own no child keys. Removing a key releases ownership without resetting its remote value.\n\n" +
		"Import initially reads all fields. Apply a narrower configuration to release unwanted fields without changing them. " +
		"Destroy leaves the configuration in place. Use one resource per plugin. " +
		"Jellyfin has no conditional configuration writes, so changes made by another writer between the read and write can be overwritten."
	resp.Schema = schema.Schema{
		Description:         description,
		MarkdownDescription: description,
		Attributes: map[string]schema.Attribute{
			"plugin_id": pluginIDAttribute(),
			"id": schema.StringAttribute{
				Description:         "The plugin configuration resource identifier.",
				MarkdownDescription: "The plugin configuration resource identifier.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"configuration_keys": schema.StringAttribute{
				Description: "Owned object keys, derived from configuration_json. Nested objects retain their structure; arrays, scalars and null are represented by null. This read-only ownership record survives remote field removal and type changes.",
				Computed:    true,
				CustomType:  jsontypes.NormalizedType{},
			},
			"configuration_json": schema.StringAttribute{
				Description: "A JSON object containing only the plugin settings to manage. Unspecified object keys are preserved; arrays are replaced whole. Sensitive values remain in Terraform state.",
				Sensitive:   true,
				Validators:  []validator.String{configurationObjectValidator{}},
				Required:    true,
				CustomType:  jsontypes.NormalizedType{},
			},
		},
	}
}

func (r *PluginConfigurationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configuredClient(req.ProviderData, "Resource", &resp.Diagnostics)
}

func (r *PluginConfigurationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	r.write(ctx, req.Plan, &resp.Diagnostics, &resp.State)
}

func (r *PluginConfigurationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data PluginConfigurationResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	configJSON, err := r.client.GetPluginConfiguration(ctx, data.PluginID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read plugin configuration", configurationError(err))
		return
	}

	// Read has no configuration. Keep the declared shape separately from
	// observed values, including for clients that do not retain private state.
	if data.ConfigurationKeys.IsNull() || data.ConfigurationKeys.IsUnknown() {
		managed := data.Configuration.ValueString()
		if managed == "" {
			managed = configJSON // Import starts with the full server document.
		}
		keys, err := configurationOwnership(managed)
		if err != nil {
			resp.Diagnostics.AddError("Invalid configuration ownership", err.Error())
			return
		}
		data.ConfigurationKeys = jsontypes.NewNormalizedValue(string(keys))
	}
	managed, err := configurationObject(data.ConfigurationKeys.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read plugin configuration", fmt.Sprintf("parsing the configuration: %s", err))
		return
	}
	served, err := servedConfiguration(configJSON, managed)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read plugin configuration", configurationError(err))
		return
	}

	data.Configuration = jsontypes.NewNormalizedValue(served)
	data.ID = data.PluginID

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *PluginConfigurationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	r.write(ctx, req.Plan, &resp.Diagnostics, &resp.State)
}

// ModifyPlan derives ownership even when releasing an absent key would
// otherwise leave the observed configuration unchanged.
func (r *PluginConfigurationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var configured jsontypes.Normalized
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("configuration_json"), &configured)...)
	if resp.Diagnostics.HasError() {
		return
	}
	keys := jsontypes.NewNormalizedUnknown()
	if !configured.IsUnknown() && !configured.IsNull() {
		shape, err := configurationOwnership(configured.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("configuration_json"), "Invalid plugin configuration", err.Error())
			return
		}
		keys = jsontypes.NewNormalizedValue(string(shape))
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("configuration_keys"), keys)...)
}

// write overlays owned fields on a fresh server document before replacing it.
func (r *PluginConfigurationResource) write(ctx context.Context, plan tfsdk.Plan, diags *diag.Diagnostics, state *tfsdk.State) {
	var data PluginConfigurationResourceModel
	diags.Append(plan.Get(ctx, &data)...)
	if diags.HasError() {
		return
	}

	desired, err := configurationObject(data.Configuration.ValueString())
	if err != nil {
		diags.AddError("Invalid plugin configuration", err.Error())
		return
	}
	keys, err := json.Marshal(ownershipShape(desired))
	if err != nil {
		diags.AddError("Invalid plugin configuration", err.Error())
		return
	}
	data.ConfigurationKeys = jsontypes.NewNormalizedValue(string(keys))

	current, err := r.client.GetPluginConfiguration(ctx, data.PluginID.ValueString())
	if err != nil {
		diags.AddError("Failed to read plugin configuration before update", configurationError(err))
		return
	}
	merged, err := mergeConfiguration(current, desired)
	if err != nil {
		diags.AddError("Invalid plugin configuration", configurationError(err))
		return
	}
	configJSON := current
	normalized, err := normalizeJSON(current)
	if err != nil {
		diags.AddError("Invalid plugin configuration", configurationError(err))
		return
	}
	if merged != normalized {
		if err := r.client.UpdatePluginConfiguration(ctx, data.PluginID.ValueString(), merged); err != nil {
			diags.AddError("Failed to update plugin configuration", configurationError(err))
			return
		}
		configJSON, err = r.client.GetPluginConfiguration(ctx, data.PluginID.ValueString())
		if err != nil {
			diags.AddError("Failed to read plugin configuration after update", configurationError(err))
			return
		}
	}
	served, err := servedConfiguration(configJSON, desired)
	if err != nil {
		diags.AddError("Failed to read plugin configuration after update", configurationError(err))
		return
	}
	data.Configuration = jsontypes.NewNormalizedValue(served)
	data.ID = data.PluginID

	diags.Append(state.Set(ctx, &data)...)
}

func (r *PluginConfigurationResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Plugin configuration cannot truly be deleted — it resets when the plugin is uninstalled.
	// We simply remove it from state.
}

func (r *PluginConfigurationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("plugin_id"), req, resp)
}

func pluginIDAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Description:         "The plugin ID (GUID), with or without dashes. Both spellings name the same plugin, so switching between them plans no change.",
		MarkdownDescription: "The plugin ID (GUID), with or without dashes. Both spellings name the same plugin, so switching between them plans no change.",
		Required:            true,
		Validators:          requiredIdentifierValidators(),
		PlanModifiers: []planmodifier.String{
			samePluginGUIDPlanModifier{},
			stringplanmodifier.RequiresReplace(),
		},
	}
}

// normalizeGUID returns a lowercase, dash-free GUID so that the provider can
// compare IDs regardless of whether Jellyfin returns them as "D" or "N" format.
func normalizeGUID(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "-", ""))
}

// samePluginGUIDPlanModifier plans the state's plugin_id when the configuration
// spells the same GUID another way, so a dashed GUID, as in an import ID, does
// not replace the resource.
type samePluginGUIDPlanModifier struct{}

func (samePluginGUIDPlanModifier) Description(context.Context) string {
	return "Keeps the plugin_id in state when the configuration spells the same GUID another way."
}

func (m samePluginGUIDPlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (samePluginGUIDPlanModifier) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.StateValue.IsNull() || req.PlanValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	if normalizeGUID(req.PlanValue.ValueString()) == normalizeGUID(req.StateValue.ValueString()) {
		resp.PlanValue = req.StateValue
	}
}

// normalizeJSON re-encodes JSON to remove insignificant formatting and sort object keys.
func normalizeJSON(raw string) (string, error) {
	normalized, err := normalizeJSONRecursive(json.RawMessage(raw), 0)
	if err != nil {
		return "", fmt.Errorf("parsing JSON for normalization: %w", err)
	}
	return string(normalized), nil
}

const maxJSONNormalizeDepth = 100

func normalizeJSONRecursive(raw json.RawMessage, depth int) (json.RawMessage, error) {
	if depth > maxJSONNormalizeDepth {
		return nil, fmt.Errorf("JSON nesting exceeds maximum depth of %d", maxJSONNormalizeDepth)
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}

	trimmed := bytes.TrimSpace(compact.Bytes())
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty JSON value")
	}

	switch trimmed[0] {
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return nil, err
		}
		for key, value := range object {
			normalized, err := normalizeJSONRecursive(value, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = normalized
		}
		return json.Marshal(object)
	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
		for i, value := range list {
			normalized, err := normalizeJSONRecursive(value, depth+1)
			if err != nil {
				return nil, err
			}
			list[i] = normalized
		}
		return json.Marshal(list)
	}

	var rawValue json.RawMessage
	if err := json.Unmarshal(trimmed, &rawValue); err != nil {
		return nil, err
	}
	return json.Marshal(rawValue)
}
