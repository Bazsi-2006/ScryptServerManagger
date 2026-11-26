package config

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var categoryIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidCategoryID reports whether id is a well-formed category identifier.
func ValidCategoryID(id string) bool { return categoryIDRe.MatchString(id) }

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// ValidateCategories checks the category registry and returns one message
// per problem.
func ValidateCategories(cats []Category) []string {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }
	if len(cats) > 500 {
		add("categories must have at most 500 entries")
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for i, c := range cats {
		f := fmt.Sprintf("categories[%d]", i)
		if !ValidCategoryID(c.ID) {
			add("%s.id must be 1-63 characters of a-z, 0-9, '_' or '-' and start with a letter or digit", f)
		} else if ids[c.ID] {
			add("%s.id %q is used twice", f, c.ID)
		}
		ids[c.ID] = true
		name := strings.TrimSpace(c.Name)
		switch {
		case name == "":
			add("%s.name is required", f)
		case len(c.Name) > 60 || hasControl(c.Name):
			add("%s.name must be at most 60 characters without control characters", f)
		case names[strings.ToLower(name)]:
			add("%s.name %q is used twice", f, name)
		}
		names[strings.ToLower(name)] = true
		if len(c.Description) > 500 || hasControl(c.Description) {
			add("%s.description must be at most 500 characters without control characters", f)
		}
		if c.MaxConcurrent < 0 || c.MaxConcurrent > 10000 {
			add("%s.max_concurrent must be between 0 (unlimited) and 10000", f)
		}
	}
	return p
}

// ResolveCategory finds the registered category a server definition refers
// to. The value matches a category ID exactly or, for definitions written
// before categories were managed, a category name case-insensitively.
func (c *Config) ResolveCategory(value string) (*Category, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	for i := range c.Categories {
		if c.Categories[i].ID == value {
			return &c.Categories[i], true
		}
	}
	for i := range c.Categories {
		if strings.EqualFold(strings.TrimSpace(c.Categories[i].Name), value) {
			return &c.Categories[i], true
		}
	}
	return nil, false
}

// CategoryByID returns the registered category with this ID.
func (c *Config) CategoryByID(id string) (*Category, bool) {
	for i := range c.Categories {
		if c.Categories[i].ID == id {
			return &c.Categories[i], true
		}
	}
	return nil, false
}

// CategoryLabel returns the display name for a definition's category value:
// the registered name, or the raw value for unmanaged categories.
func (c *Config) CategoryLabel(value string) string {
	if cat, ok := c.ResolveCategory(value); ok {
		return cat.Name
	}
	return strings.TrimSpace(value)
}
