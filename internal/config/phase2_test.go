package config

import (
	"strings"
	"testing"
	"time"
)

func TestCategoriesValidateAndResolve(t *testing.T) {
	c := Default()
	c.applyPathDefaults("/etc/server-manager")
	c.Categories = []Category{
		{ID: "minecraft", Name: "Minecraft", MaxConcurrent: 2},
		{ID: "source", Name: "Source Engine", MaxConcurrent: 3},
		{ID: "other", Name: "Other Games"},
	}
	c.Limits.MaxConcurrentServers = 4
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for value, want := range map[string]string{"minecraft": "minecraft", "Minecraft": "minecraft", " source engine ": "source", "other": "other"} {
		got, ok := c.ResolveCategory(value)
		if !ok || got.ID != want {
			t.Errorf("ResolveCategory(%q) = %v, %v; want %s", value, got, ok, want)
		}
	}
	if _, ok := c.ResolveCategory("Factorio"); ok {
		t.Error("an unmanaged category must not resolve")
	}
	if _, ok := c.ResolveCategory(""); ok {
		t.Error("no category must not resolve")
	}
	if c.CategoryLabel("minecraft") != "Minecraft" || c.CategoryLabel("Factorio") != "Factorio" {
		t.Error("labels")
	}

	bad := map[string][]Category{
		"bad id":         {{ID: "Mine Craft", Name: "x"}},
		"duplicate id":   {{ID: "a", Name: "A"}, {ID: "a", Name: "B"}},
		"duplicate name": {{ID: "a", Name: "Same"}, {ID: "b", Name: "same"}},
		"no name":        {{ID: "a", Name: " "}},
		"long name":      {{ID: "a", Name: strings.Repeat("n", 61)}},
		"negative limit": {{ID: "a", Name: "A", MaxConcurrent: -1}},
	}
	for name, cats := range bad {
		if len(ValidateCategories(cats)) == 0 {
			t.Errorf("%s: should be rejected", name)
		}
	}
}

func TestNewSettingsHaveSafeDefaultsAndBounds(t *testing.T) {
	c := Default()
	if c.Limits.MaxConcurrentServers != 0 || len(c.Categories) != 0 {
		t.Error("limits must default to unlimited")
	}
	if !c.Notifications.Enabled || c.Notifications.Cooldown.D() != 10*time.Minute || c.Auth.PasswordResetTTL.D() != time.Hour {
		t.Errorf("defaults: %+v %v", c.Notifications, c.Auth.PasswordResetTTL)
	}
	for name, doc := range map[string]string{
		"negative global limit": `{"limits":{"max_concurrent_servers":-1}}`,
		"reset ttl too short":   `{"auth":{"password_reset_ttl":"1m"}}`,
		"reset ttl too long":    `{"auth":{"password_reset_ttl":"48h"}}`,
		"bad action rate":       `{"rate_limits":{"action_run":{"requests":0,"window":"1m"}}}`,
		"bad category":          `{"categories":[{"id":"Bad ID","name":"x"}]}`,
		"negative cooldown":     `{"notifications":{"cooldown":"-1s"}}`,
	} {
		if _, err := Parse([]byte(doc), "/etc/server-manager"); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	// A configuration written by the previous release (no new sections)
	// still loads, with the new defaults.
	old := `{"mode":"production","listen":"127.0.0.1:8080","rate_limits":{"login":{"requests":5,"window":"1m"}}}`
	got, err := Parse([]byte(old), "/etc/server-manager")
	if err != nil {
		t.Fatal(err)
	}
	if got.RateLimits.ActionRun.Requests != 20 || got.RateLimits.PasswordReset.Requests != 5 || !got.Notifications.Enabled {
		t.Errorf("older configuration did not get the new defaults: %+v", got.RateLimits)
	}
}
